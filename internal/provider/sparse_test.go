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

package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.ate.dev/substrate/api/objectstore/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// denseHeaderSizeV1 is the encoding overhead of a file without holes, which
// encodes as one frame.
const denseHeaderSizeV1 = sparseFixedHeaderV1 + sparseFrameHeaderV1

// testFormat is the format metadata of the blobs tests seed.
var testFormat = sparseFormatV1

// frame is a populated range of a file the tests encode.
type frame struct {
	off  int64
	data string
}

// encodeFramesV1 encodes a file of logical bytes holding frames, as an upload
// does, without checking that the frames are valid.
func encodeFramesV1(logical int64, frames ...frame) []byte {
	header := sparseHeaderV1(logical, len(frames))
	encoded := header[:]
	for _, f := range frames {
		position := frameHeaderV1(extent{off: f.off, length: int64(len(f.data))})
		encoded = append(append(encoded, position[:]...), f.data...)
	}
	return encoded
}

// denseEncodingV1 returns data encoded as an upload encodes a file without
// holes.
func denseEncodingV1(data []byte) []byte {
	if len(data) == 0 {
		return encodeFramesV1(0)
	}
	return encodeFramesV1(int64(len(data)), frame{off: 0, data: string(data)})
}

// denseDigestV1 returns the hex SHA-256 of denseEncodingV1(data).
func denseDigestV1(data []byte) string {
	sum := sha256.Sum256(denseEncodingV1(data))
	return hex.EncodeToString(sum[:])
}

// sparseBlob returns data as a committed snapshot blob, with the format and
// SHA-256 metadata an upload commits. A copy never decodes, so its tests may
// pass any bytes.
func sparseBlob(data []byte) fakeBlob {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	format := sparseFormatV1
	return fakeBlob{data: data, metadata: map[string]*string{integrityMetadata: &digest, formatMetadata: &format}}
}

// TestFetchSnapshotRejectsMalformedBlobs checks that every malformed blob
// fails the fetch with DataLoss, leaves the existing destination file as it
// was, and leaves no temporary file behind.
func TestFetchSnapshotRejectsMalformedBlobs(t *testing.T) {
	const logical = 10
	valid := encodeFramesV1(logical, frame{off: 2, data: "abc"})
	patch := func(off int, field []byte) []byte {
		b := slices.Clone(valid)
		copy(b[off:], field)
		return b
	}
	u32 := func(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }
	u64 := func(v uint64) []byte { return binary.LittleEndian.AppendUint64(nil, v) }
	withoutMetadata := func(key string) fakeBlob {
		b := sparseBlob(valid)
		delete(b.metadata, key)
		return b
	}
	unknownFormat := sparseBlob(valid)
	other := "sparse-v2"
	unknownFormat.metadata[formatMetadata] = &other
	mismatched := sparseBlob(valid)
	mismatched.data = encodeFramesV1(logical, frame{off: 2, data: "abd"})
	twoFrames := encodeFramesV1(logical, frame{off: 2, data: "abc"}, frame{off: 7, data: "d"})

	for _, tc := range []struct {
		name string
		blob fakeBlob
	}{
		{"no format metadata", withoutMetadata(formatMetadata)},
		{"unknown format metadata", unknownFormat},
		{"no SHA-256 metadata", withoutMetadata(integrityMetadata)},
		{"SHA-256 mismatch", mismatched},
		{"empty blob", sparseBlob(nil)},
		{"truncated header", sparseBlob(valid[:20])},
		{"wrong magic", sparseBlob(patch(0, []byte("NOTSPARS")))},
		{"unsupported version", sparseBlob(patch(8, u32(2)))},
		{"flags set", sparseBlob(patch(12, u32(1)))},
		{"logical size overflows", sparseBlob(patch(16, u64(math.MaxInt64+1)))},
		{"more frames than the blob holds", sparseBlob(patch(24, u64(1000)))},
		{"frame offset overflows", sparseBlob(patch(32, u64(math.MaxUint64)))},
		{"frame length overflows", sparseBlob(patch(40, u64(math.MaxUint64)))},
		{"empty frame", sparseBlob(encodeFramesV1(logical, frame{off: 2}, frame{off: 4, data: "a"}))},
		{"overlapping frames", sparseBlob(encodeFramesV1(logical, frame{off: 2, data: "abc"}, frame{off: 4, data: "d"}))},
		{"unsorted frames", sparseBlob(encodeFramesV1(logical, frame{off: 6, data: "a"}, frame{off: 2, data: "b"}))},
		{"frame past the file", sparseBlob(encodeFramesV1(logical, frame{off: 8, data: "abc"}))},
		{"short frame data", sparseBlob(valid[:len(valid)-1])},
		{"missing frame", sparseBlob(twoFrames[:len(valid)])},
		{"truncated frame header", sparseBlob(twoFrames[:len(valid)+8])},
		{"trailing bytes", sparseBlob(append(slices.Clone(valid), 'x'))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dest := filepath.Join(root, "dest")
			if err := os.Mkdir(dest, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dest, "memory.img"), []byte("previous"), 0o600); err != nil {
				t.Fatal(err)
			}
			store := newFakeStore()
			store.blobs["tenant/actors/a/snapshots/one/memory.img"] = tc.blob
			server := newTestServer(t, store, root)
			_, err := server.FetchSnapshot(context.Background(), &objectstore.FetchSnapshotRequest{
				SnapshotUri: testURI, WritePath: dest, Files: []string{"memory.img"},
			})
			if status.Code(err) != codes.DataLoss {
				t.Fatalf("FetchSnapshot code = %v, want DataLoss: %v", status.Code(err), err)
			}
			if got, err := os.ReadFile(filepath.Join(dest, "memory.img")); err != nil || string(got) != "previous" {
				t.Fatalf("destination = %q (err %v), want it unchanged", got, err)
			}
			if entries, err := os.ReadDir(dest); err != nil || len(entries) != 1 {
				t.Fatalf("destination directory holds %v (err %v), want no temporary file left", entries, err)
			}
		})
	}

	t.Run("valid", func(t *testing.T) {
		root := t.TempDir()
		store := newFakeStore()
		store.blobs["tenant/actors/a/snapshots/one/memory.img"] = sparseBlob(valid)
		server := newTestServer(t, store, root)
		if _, err := server.FetchSnapshot(context.Background(), &objectstore.FetchSnapshotRequest{
			SnapshotUri: testURI, WritePath: root, Files: []string{"memory.img"},
		}); err != nil {
			t.Fatalf("FetchSnapshot: %v", err)
		}
		want := []byte("\x00\x00abc\x00\x00\x00\x00\x00")
		if got, err := os.ReadFile(filepath.Join(root, "memory.img")); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("restored %q (err %v), want %q", got, err, want)
		}
	})
}
