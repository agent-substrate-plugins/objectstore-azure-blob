// Copyright 2026 The Agent Substrate Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package blobstore defines the Azure Blob operations used by the plugin.
package blobstore

import (
	"context"
	"errors"
	"io"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
)

// Azure's limits on a committed block blob. The values come from the vendored
// SDK (azblob v1.8.0 blockblob.MaxBlocks and MaxStageBlockBytes: 50,000
// blocks of at most 4,000 MiB) and match the service's scale targets for
// service version 2019-12-12 and later, which the SDK's 2026-06-06 requests
// use:
// https://learn.microsoft.com/azure/storage/blobs/scalability-targets
const (
	// MaxBlocks is the most blocks one block blob may commit.
	MaxBlocks = blockblob.MaxBlocks
	// MaxBlockSize is the most bytes one StageBlock call may send.
	MaxBlockSize = blockblob.MaxStageBlockBytes
)

var (
	// ErrNotFound reports an absent blob.
	ErrNotFound = errors.New("blob not found")
	// ErrContainerNotFound reports a configured container that does not exist.
	ErrContainerNotFound = errors.New("blob container not found")
	// ErrPermission reports rejected Azure credentials or authorization.
	ErrPermission = errors.New("blob access denied")
	// ErrUnavailable reports a retryable Azure service failure.
	ErrUnavailable = errors.New("blob service unavailable")
)

// Download is a blob body, its size and its metadata.
type Download struct {
	Body io.ReadCloser
	// Size is the blob's length in bytes as the service reported it, or
	// negative if it reported none.
	Size int64
	// Metadata is the blob's metadata, with lower-case names.
	Metadata map[string]string
}

// Store is the block blob surface used by the snapshot provider.
type Store interface {
	// StageBlock stages the bytes of body, at most MaxBlockSize, as one
	// uncommitted block of blob name. body must be at its start: the store
	// may seek it back there to resend it, and is done with it when
	// StageBlock returns. A block whose bytes do not hash to md5 fails with
	// ErrUnavailable, since reading the source again may succeed; a nil md5
	// stages the block unchecked.
	StageBlock(ctx context.Context, name, blockID string, body io.ReadSeeker, md5 []byte) error
	// CommitBlockList atomically replaces blob name with the staged blocks
	// in blockIDs, in that order, and sets its metadata.
	CommitBlockList(ctx context.Context, name string, blockIDs []string, metadata map[string]string) error
	// Download opens blob name. Reading the body resumes the download after
	// a dropped connection.
	Download(ctx context.Context, name string) (Download, error)
	// DownloadRange opens count bytes of a blob, starting at offset. count
	// must be positive.
	DownloadRange(ctx context.Context, name string, offset, count int64) (io.ReadCloser, error)
	// List returns the name of every blob under prefix.
	List(ctx context.Context, prefix string) ([]string, error)
	// Delete removes blob name. An absent blob is already deleted, so
	// deleting it succeeds.
	Delete(ctx context.Context, name string) error
}
