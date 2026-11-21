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

//go:build unix

package provider

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"go.ate.dev/substrate/api/objectstore/v1"
	"plugins.go.ate.dev/objectstore-azure-blob/internal/blobstore"
)

// allocationSlack is how much more than its data a restored file may
// allocate: the file system may add a metadata block or two.
const allocationSlack = 64 << 10

// randomBytes returns n bytes that do not compress, so a file system that
// compresses, such as ZFS or btrfs, allocates them in full.
func randomBytes(n int) []byte {
	data := make([]byte, n)
	rng := rand.NewChaCha8([32]byte{byte(n), byte(n >> 8), byte(n >> 16)})
	_, _ = rng.Read(data)
	return data
}

// blockSize returns the allocation unit of the file system holding dir.
func blockSize(t *testing.T, dir string) int64 {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	return int64(info.Sys().(*syscall.Stat_t).Blksize)
}

// dataBound returns the most bytes the extents holding writes can span:
// each write rounded out to whole blocks of block bytes.
func dataBound(writes map[int64][]byte, block int64) int64 {
	var bound int64
	for off, data := range writes {
		start := off / block * block
		end := (off + int64(len(data)) + block - 1) / block * block
		bound += end - start
	}
	return bound
}

// allocatedBytes returns the disk space allocated to the file at path, which
// for a sparse file is less than its length.
func allocatedBytes(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t).Blocks * 512
}

// holesSupported reports whether files in dir keep holes and the provider
// looks holes up on this platform. It does not ask dataExtents, so a scan
// that stops finding holes fails the tests instead of skipping them.
func holesSupported(t *testing.T, dir string) bool {
	t.Helper()
	if runtime.GOOS != "linux" {
		return false
	}
	probe, err := os.CreateTemp(dir, "probe")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe.Name())
	defer probe.Close()
	if err := probe.Truncate(1 << 20); err != nil {
		t.Fatal(err)
	}
	return allocatedBytes(t, probe.Name()) == 0
}

// writeSparseFile creates a file of size bytes holding data at each offset
// in writes and holes elsewhere.
func writeSparseFile(t *testing.T, path string, size int64, writes map[int64][]byte) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	err = file.Truncate(size)
	for off, data := range writes {
		_, writeErr := file.WriteAt(data, off)
		err = errors.Join(err, writeErr)
	}
	if err := errors.Join(err, file.Close()); err != nil {
		t.Fatal(err)
	}
}

// TestSparseRoundTrip uploads files with and without holes, copies them, and
// fetches the upload and the copy. Each restore must match the file byte for
// byte, keep its holes unallocated, and replace an existing destination
// without its bytes showing through the holes. The blob must hold only the
// file's populated bytes after its header.
func TestSparseRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		size   int64
		writes map[int64][]byte
	}{
		{"data and holes", 16 * mib, map[int64][]byte{4 * mib: randomBytes(2 * mib), 12 * mib: randomBytes(mib)}},
		{"leading data, trailing hole", 8 * mib, map[int64][]byte{0: randomBytes(64 << 10)}},
		{"all hole", 8 * mib, nil},
		{"unaligned length", 5*mib + 123, map[int64][]byte{mib: randomBytes(100 << 10), 5*mib + 46: randomBytes(77)}},
		{"empty", 0, nil},
		{"dense", 3*mib + 5, map[int64][]byte{0: randomBytes(3*mib + 5)}},
		{"small manifest", 17, map[int64][]byte{0: []byte(`{"files":["a"]}` + "\n\n")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			source := filepath.Join(root, "source")
			dirs := map[string]string{testURI: filepath.Join(root, "dest"), "azblob://snapshots/tags/stable": filepath.Join(root, "tagged")}
			for _, dir := range []string{source, dirs[testURI], dirs["azblob://snapshots/tags/stable"]} {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			holes := holesSupported(t, source)
			path := filepath.Join(source, "memory.img")
			writeSparseFile(t, path, tc.size, tc.writes)
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			store := newFakeStore()
			server := newTestServer(t, store, root)

			if _, err := server.UploadSnapshot(ctx, &objectstore.UploadSnapshotRequest{SnapshotUri: testURI, LocalPath: source, Files: []string{"memory.img"}}); err != nil {
				t.Fatalf("UploadSnapshot: %v", err)
			}
			blob := store.blobs["tenant/actors/a/snapshots/one/memory.img"]
			if _, err := decoderFor(blob.metadata); err != nil {
				t.Fatalf("committed metadata: %v", err)
			}
			count := binary.LittleEndian.Uint64(blob.data[24:])
			dataBytes := int64(len(blob.data)) - sparseFixedHeaderV1 - sparseFrameHeaderV1*int64(count)
			var written int64
			for _, data := range tc.writes {
				written += int64(len(data))
			}
			bound := dataBound(tc.writes, blockSize(t, source))
			if holes {
				if dataBytes < written || dataBytes > bound {
					t.Errorf("blob holds %d data bytes, want between the %d written and the %d their blocks span", dataBytes, written, bound)
				}
			} else if dataBytes != tc.size {
				t.Errorf("blob holds %d data bytes, want the file's %d", dataBytes, tc.size)
			}

			if _, err := server.CopySnapshot(ctx, &objectstore.CopySnapshotRequest{SrcUri: testURI, DstUri: "azblob://snapshots/tags/stable"}); err != nil {
				t.Fatalf("CopySnapshot: %v", err)
			}
			for uri, dest := range dirs {
				restored := filepath.Join(dest, "memory.img")
				if err := os.WriteFile(restored, bytes.Repeat([]byte{0xff}, int(tc.size)+4096), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := server.FetchSnapshot(ctx, &objectstore.FetchSnapshotRequest{SnapshotUri: uri, WritePath: dest, Files: []string{"memory.img"}}); err != nil {
					t.Fatalf("FetchSnapshot(%s): %v", uri, err)
				}
				got, err := os.ReadFile(restored)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("FetchSnapshot(%s) restored %d bytes that differ from the %d byte file", uri, len(got), len(want))
				}
				if holes {
					if got := allocatedBytes(t, restored); got > bound+allocationSlack {
						t.Errorf("FetchSnapshot(%s) allocated %d bytes, want at most the %d its data spans plus %d", uri, got, bound, allocationSlack)
					}
				}
				if entries, err := os.ReadDir(dest); err != nil || len(entries) != 1 {
					t.Fatalf("restore directory holds %v (err %v), want no temporary file left", entries, err)
				}
			}
		})
	}
}

// TestUploadSnapshotSizesSparseFileByEncoding checks that a file whose length
// exceeds the block limits uploads if its encoding fits them.
func TestUploadSnapshotSizesSparseFileByEncoding(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if !holesSupported(t, source) {
		t.Skip("the test directory's file system does not keep holes")
	}
	writeSparseFile(t, filepath.Join(source, "memory.img"), 64*mib, map[int64][]byte{32 * mib: randomBytes(mib)})
	store := &sizeRecordingStore{fakeStore: newFakeStore(), blocks: map[int]int{}}
	server, err := New(map[string]blobstore.Store{"snapshots": store}, "tenant", root)
	if err != nil {
		t.Fatal(err)
	}
	server.limits = blockLimits{maxBlocks: 2, maxBlockSize: 6 * mib}
	if _, err := server.UploadSnapshot(context.Background(), &objectstore.UploadSnapshotRequest{SnapshotUri: testURI, LocalPath: source, Files: []string{"memory.img"}}); err != nil {
		t.Fatalf("UploadSnapshot: %v", err)
	}
	if len(store.blocks) != 1 || store.blocks[0] != denseHeaderSizeV1+mib {
		t.Fatalf("staged blocks %v, want one of %d bytes", store.blocks, denseHeaderSizeV1+mib)
	}
}
