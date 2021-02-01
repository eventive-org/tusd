// Package azurestore provides a Azure BlobStorage based backend

// AzureStore is a storage backend that uses the AzService interface in order to store uploads in Azure BlobStorage.
// It stores the uploads in a container specified in two different BlockBlob: The `[id].info` blobs are used to store the fileinfo in JSON format. The `[id]` blobs without an extension contain the raw binary data uploaded.
// If the upload is not finished within a week, the uncommited blocks will be discarded.

// Possible future features:
//  - Set the access tier of the blob
//  - Change new container access
package azurestore

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	"github.com/Azure/azure-storage-blob-go/azblob"
)

const (
	InfoBlobSuffix        string = ".info"
	MaxBlockBlobSize      int64  = azblob.BlockBlobMaxBlocks * azblob.BlockBlobMaxStageBlockBytes
	MaxBlockBlobChunkSize int64  = azblob.BlockBlobMaxStageBlockBytes
)

type azService struct {
	ContainerURL  *azblob.ContainerURL
	ContainerName string
}

type AzService interface {
	NewBlob(ctx context.Context, name string) (AzBlob, error)
}

type AzConfig struct {
	AccountName      string
	AccountKey       string
	ContainerName    string
	Endpoint         string
	EndpointProtocol string
}

type AzError struct {
	error      error
	StatusCode int
	Status     string
}

type AzBlob interface {
	Delete(ctx context.Context) error
	Upload(ctx context.Context, body io.ReadSeeker) error
	Download(ctx context.Context) ([]byte, error)
	GetOffset(ctx context.Context) (int64, error)
	Commit(ctx context.Context) error
}

type BlockBlob struct {
	Blob    *azblob.BlockBlobURL
	Indexes []int
}

type InfoBlob struct {
	Blob *azblob.BlockBlobURL
}

func (a AzError) Error() string {
	return a.error.Error()
}

// New Azure service for communication to Azure BlockBlob Storage API
func NewAzureService(config *AzConfig) (AzService, error) {
	// struct to store your credentials.
	credential, err := azblob.NewSharedKeyCredential(config.AccountName, config.AccountKey)
	if err != nil {
		return nil, err
	}

	// The pipeline specifies things like retry policies, logging, deserialization of HTTP response payloads, and more.
	p := azblob.NewPipeline(credential, azblob.PipelineOptions{})
	cURL, _ := url.Parse(fmt.Sprintf("%s/%s", config.Endpoint, config.ContainerName))

	// Instantiate a new ContainerURL, and a new BlobURL object to run operations on container (Create) and blobs (Upload and Download).
	// Get the ContainerURL URL
	containerURL := azblob.NewContainerURL(*cURL, p)
	// Do not care about response since it will fail if container exists and create if it does not.
	_, _ = containerURL.Create(context.Background(), azblob.Metadata{}, azblob.PublicAccessNone)

	return &azService{
		ContainerURL:  &containerURL,
		ContainerName: config.ContainerName,
	}, nil
}

func (service *azService) NewBlob(ctx context.Context, name string) (AzBlob, error) {
	var fileBlob AzBlob
	bb := service.ContainerURL.NewBlockBlobURL(name)
	if strings.HasSuffix(name, InfoBlobSuffix) {
		fileBlob = &InfoBlob{
			Blob: &bb,
		}
	} else {
		fileBlob = &BlockBlob{
			Blob:    &bb,
			Indexes: []int{},
		}
	}
	return fileBlob, nil
}

func (blockBlob *BlockBlob) Delete(ctx context.Context) error {
	_, err := blockBlob.Blob.Delete(ctx, azblob.DeleteSnapshotsOptionInclude, azblob.BlobAccessConditions{})
	return err
}

func (blockBlob *BlockBlob) Upload(ctx context.Context, body io.ReadSeeker) error {
	// Keep track of the indexes
	var index int
	if len(blockBlob.Indexes) == 0 {
		index = 0
	} else {
		index = blockBlob.Indexes[len(blockBlob.Indexes)-1] + 1
	}
	blockBlob.Indexes = append(blockBlob.Indexes, index)

	_, err := blockBlob.Blob.StageBlock(ctx, blockIDIntToBase64(index), body,
		azblob.LeaseAccessConditions{},
		nil,
		azblob.ClientProvidedKeyOptions{})
	if err != nil {
		return err
	}
	return nil
}

func (blockBlob *BlockBlob) Download(ctx context.Context) ([]byte, error) {
	downloadResponse, err := blockBlob.Blob.Download(ctx, 0, azblob.CountToEnd, azblob.BlobAccessConditions{}, false, azblob.ClientProvidedKeyOptions{})

	if downloadResponse != nil && downloadResponse.StatusCode() == 404 {
		url := blockBlob.Blob.URL()
		return nil, &AzError{
			error:      fmt.Errorf("File %s does not exist", url.String()),
			StatusCode: downloadResponse.StatusCode(),
			Status:     downloadResponse.Status(),
		}
	}
	if err != nil {
		return nil, err
	}

	bodyStream := downloadResponse.Body(azblob.RetryReaderOptions{MaxRetryRequests: 20})
	downloadedData := bytes.Buffer{}

	_, err = downloadedData.ReadFrom(bodyStream)
	if err != nil {
		return nil, err
	}

	return downloadedData.Bytes(), nil
}

func (blockBlob *BlockBlob) GetOffset(ctx context.Context) (int64, error) {
	// Get the offset of the file from azure storage
	// For the blob, show each block (ID and size) that is a committed part of it.
	var indexes []int
	var offset int64
	offset = 0

	getBlock, err := blockBlob.Blob.GetBlockList(ctx, azblob.BlockListAll, azblob.LeaseAccessConditions{})
	if err != nil {
		// Check the error response, and see if the error contains ServiceCode=BlobNotFound
		// This means the blob is not found, so we can say the offset is 0
		if strings.Contains(err.Error(), "ServiceCode=BlobNotFound") {
			return 0, nil
		}

		return 0, err
	}

	// Need committed blocks to be added to offset to know how big the file really is
	for _, block := range getBlock.CommittedBlocks {
		offset += int64(block.Size)
		indexes = append(indexes, blockIDBase64ToInt(block.Name))
	}

	// Need to get the uncommitted blocks so that we can commit them
	for _, block := range getBlock.UncommittedBlocks {
		offset += int64(block.Size)
		indexes = append(indexes, blockIDBase64ToInt(block.Name))
	}

	sort.Ints(indexes)
	blockBlob.Indexes = indexes

	return offset, nil
}

func (blockBlob *BlockBlob) Commit(ctx context.Context) error {
	// After all the blocks are uploaded, commit them to the blob.
	base64BlockIDs := make([]string, len(blockBlob.Indexes))
	for index, id := range blockBlob.Indexes {
		base64BlockIDs[index] = blockIDIntToBase64(id)
	}

	_, err := blockBlob.Blob.CommitBlockList(ctx, base64BlockIDs, azblob.BlobHTTPHeaders{}, azblob.Metadata{}, azblob.BlobAccessConditions{}, azblob.DefaultAccessTier, nil, azblob.ClientProvidedKeyOptions{})
	return err
}

func (infoBlob *InfoBlob) Delete(ctx context.Context) error {
	_, err := infoBlob.Blob.Delete(ctx, azblob.DeleteSnapshotsOptionInclude, azblob.BlobAccessConditions{})
	return err
}

func (infoBlob *InfoBlob) Upload(ctx context.Context, body io.ReadSeeker) error {
	_, err := infoBlob.Blob.Upload(ctx, body, azblob.BlobHTTPHeaders{}, azblob.Metadata{}, azblob.BlobAccessConditions{}, azblob.DefaultAccessTier, nil, azblob.ClientProvidedKeyOptions{})
	return err
}

func (infoBlob *InfoBlob) Download(ctx context.Context) ([]byte, error) {
	downloadResponse, err := infoBlob.Blob.Download(ctx, 0, azblob.CountToEnd, azblob.BlobAccessConditions{}, false, azblob.ClientProvidedKeyOptions{})

	if downloadResponse != nil && downloadResponse.StatusCode() == 404 {
		url := infoBlob.Blob.URL()
		return nil, &AzError{
			error:      fmt.Errorf("File %s does not exist", url.String()),
			StatusCode: downloadResponse.StatusCode(),
			Status:     downloadResponse.Status(),
		}
	}
	if err != nil {
		return nil, err
	}

	bodyStream := downloadResponse.Body(azblob.RetryReaderOptions{MaxRetryRequests: 20})
	downloadedData := bytes.Buffer{}

	_, err = downloadedData.ReadFrom(bodyStream)
	if err != nil {
		return nil, err
	}

	return downloadedData.Bytes(), nil
}

func (infoBlob *InfoBlob) GetOffset(ctx context.Context) (int64, error) {
	return 0, nil
}

func (infoBlob *InfoBlob) Commit(ctx context.Context) error {
	return nil
}

// === Helper Functions ===
// These helper functions convert a binary block ID to a base-64 string and vice versa
// NOTE: The blockID must be <= 64 bytes and ALL blockIDs for the block must be the same length
func blockIDBinaryToBase64(blockID []byte) string {
	return base64.StdEncoding.EncodeToString(blockID)
}

func blockIDBase64ToBinary(blockID string) []byte {
	binary, _ := base64.StdEncoding.DecodeString(blockID)
	return binary
}

// These helper functions convert an int block ID to a base-64 string and vice versa
func blockIDIntToBase64(blockID int) string {
	binaryBlockID := (&[4]byte{})[:] // All block IDs are 4 bytes long
	binary.LittleEndian.PutUint32(binaryBlockID, uint32(blockID))
	return blockIDBinaryToBase64(binaryBlockID)
}

func blockIDBase64ToInt(blockID string) int {
	blockIDBase64ToBinary(blockID)
	return int(binary.LittleEndian.Uint32(blockIDBase64ToBinary(blockID)))
}
