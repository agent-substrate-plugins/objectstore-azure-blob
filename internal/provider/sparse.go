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
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
)

// A snapshot blob holds its file in a sparse format, so the file's holes
// cross Azure as gaps between ranges instead of as zeros, and a restore leaves
// them unallocated. Version 1 of the format, sparseFormatV1, is:
//
//	magic[8] | version:u32 | flags:u32 | logicalSize:u64 | count:u64 |
//	(offset:u64 | length:u64 | data[length]) * count
//
// Integers are little-endian. Each frame holds one of the file's populated
// extents, in order, each non-empty and none overlapping. Every byte outside
// the frames is zero. A frame's position precedes its data, so a decoder
// writes each frame as it reads it and holds no table of them. The layout
// follows substrate's pkg/objectstorage sparse-extent format, without its
// zstd compression, and is not compatible with it.
//
// The blob's formatMetadata names the format, and its integrityMetadata is the
// SHA-256 of the blob's bytes, so verifying or copying a blob never decodes it.
//
// Names that belong to one format version end in it, such as decodeSparseV1.
// A new version adds its own (sparseFormatV2, sparseVersionV2,
// decodeSparseV2), a case in decoderFor, and a new encoder for uploads;
// readers keep every earlier version's decoder. Names without a version, such
// as sparseMagic and formatMetadata, are shared by every version.
const (
	sparseMagic                 = "AZSPARSE"
	sparseVersionV1      uint32 = 1
	sparseFixedHeaderV1         = 32
	sparseFrameHeaderV1         = 16
	formatMetadata              = "substrate_format"
	sparseFormatV1              = "sparse-v1"
	decodeReadBufferSize        = 64 << 10
	decodeCopyBufferSize        = 1 << 20
)

// snapshotDecoder restores a file into dst, a new empty file, from the bytes
// of its blob read from r. size is the blob's length as the service reported
// it, or negative if unknown. A malformed blob fails with errIntegrity; a
// failure to read r or write dst keeps its own error.
type snapshotDecoder func(dst *os.File, r io.Reader, size int64) error

// decoderFor returns the decoder for the snapshot format metadata names. A
// new format adds its decoder here; copies never decode, so they need none.
func decoderFor(metadata map[string]*string) (snapshotDecoder, error) {
	format, ok := metadataValue(metadata, formatMetadata)
	if !ok {
		return nil, fmt.Errorf("%w: blob has no snapshot format metadata", errIntegrity)
	}
	switch format {
	case sparseFormatV1:
		return decodeSparseV1, nil
	default:
		return nil, fmt.Errorf("%w: snapshot format %q is not supported", errIntegrity, format)
	}
}

// extent is a populated byte range of a snapshot file.
type extent struct{ off, length int64 }

// wholeFile returns the extents of a file of size bytes without holes.
func wholeFile(size int64) []extent {
	if size == 0 {
		return nil
	}
	return []extent{{off: 0, length: size}}
}

// sparseHeaderV1 returns the fixed header of a file of logicalSize bytes with
// count populated extents.
func sparseHeaderV1(logicalSize int64, count int) [sparseFixedHeaderV1]byte {
	var header [sparseFixedHeaderV1]byte
	copy(header[:], sparseMagic)
	binary.LittleEndian.PutUint32(header[8:], sparseVersionV1)
	binary.LittleEndian.PutUint64(header[16:], uint64(logicalSize))
	binary.LittleEndian.PutUint64(header[24:], uint64(count))
	return header
}

// frameHeaderV1 returns the position of e that precedes its data.
func frameHeaderV1(e extent) [sparseFrameHeaderV1]byte {
	var header [sparseFrameHeaderV1]byte
	binary.LittleEndian.PutUint64(header[:], uint64(e.off))
	binary.LittleEndian.PutUint64(header[8:], uint64(e.length))
	return header
}

// sparseFileV1 is the sparse encoding of a snapshot file. Its ReadAt reads the
// encoding at any offset from the headers and the file's extents, so an
// upload can hash and stage each block straight from the file.
type sparseFileV1 struct {
	file   *os.File
	header [sparseFixedHeaderV1]byte
	exts   []extent
	// starts[i] is the offset of exts[i]'s frame in the encoding.
	starts []int64
	// size is the encoding's length.
	size int64
}

// newSparseFileV1 maps the populated extents of file, which is logicalSize
// bytes long. The file must not change until the encoding has been read.
func newSparseFileV1(file *os.File, logicalSize int64) (*sparseFileV1, error) {
	exts, err := dataExtents(file, logicalSize)
	if err != nil {
		return nil, err
	}
	// A file that shrank during the scan would otherwise encode its lost
	// tail as zeros. Later changes fail the upload's block checks.
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() != logicalSize {
		return nil, fmt.Errorf("file changed from %d to %d bytes while its holes were mapped", logicalSize, info.Size())
	}
	starts := make([]int64, len(exts))
	size := int64(sparseFixedHeaderV1)
	for i, e := range exts {
		starts[i] = size
		size += sparseFrameHeaderV1 + e.length
	}
	return &sparseFileV1{file: file, header: sparseHeaderV1(logicalSize, len(exts)), exts: exts, starts: starts, size: size}, nil
}

func (s *sparseFileV1) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("read at negative offset %d", off)
	}
	var n int
	for len(p) > 0 {
		if off >= s.size {
			return n, io.EOF
		}
		if off < sparseFixedHeaderV1 {
			copied := copy(p, s.header[off:])
			n += copied
			p = p[copied:]
			off += int64(copied)
			continue
		}
		i := sort.Search(len(s.exts), func(i int) bool { return s.starts[i]+sparseFrameHeaderV1+s.exts[i].length > off })
		e := s.exts[i]
		within := off - s.starts[i]
		if within < sparseFrameHeaderV1 {
			header := frameHeaderV1(e)
			copied := copy(p, header[within:])
			n += copied
			p = p[copied:]
			off += int64(copied)
			continue
		}
		within -= sparseFrameHeaderV1
		want := min(int64(len(p)), e.length-within)
		read, err := s.file.ReadAt(p[:want], e.off+within)
		n += read
		p = p[read:]
		off += int64(read)
		if errors.Is(err, io.EOF) {
			return n, fmt.Errorf("snapshot file ended inside the %d byte extent at %d: %w", e.length, e.off, io.ErrUnexpectedEOF)
		}
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// decodeSparseV1 is the snapshotDecoder of sparseFormatV1. dst must be empty:
// only the frames are written, so the rest of dst stays a hole and reads as
// zeros. It holds one frame's position at a time, whatever the blob claims.
func decodeSparseV1(dst *os.File, r io.Reader, size int64) error {
	br := bufio.NewReaderSize(r, decodeReadBufferSize)
	var fixed [sparseFixedHeaderV1]byte
	if err := readSparseHeader(br, fixed[:]); err != nil {
		return err
	}
	if string(fixed[:len(sparseMagic)]) != sparseMagic {
		return fmt.Errorf("%w: blob does not start with the sparse snapshot magic", errIntegrity)
	}
	if version := binary.LittleEndian.Uint32(fixed[8:]); version != sparseVersionV1 {
		return fmt.Errorf("%w: sparse snapshot version %d is not supported", errIntegrity, version)
	}
	if flags := binary.LittleEndian.Uint32(fixed[12:]); flags != 0 {
		return fmt.Errorf("%w: sparse snapshot flags %#x are not supported", errIntegrity, flags)
	}
	logical := binary.LittleEndian.Uint64(fixed[16:])
	if logical > math.MaxInt64 {
		return fmt.Errorf("%w: sparse snapshot logical size %d is too large", errIntegrity, logical)
	}
	count := binary.LittleEndian.Uint64(fixed[24:])
	// A frame holds at least one byte after its position.
	if size >= 0 && (size < sparseFixedHeaderV1 || count > uint64(size-sparseFixedHeaderV1)/(sparseFrameHeaderV1+1)) {
		return fmt.Errorf("%w: %d sparse frames do not fit in a %d byte blob", errIntegrity, count, size)
	}

	if err := dst.Truncate(int64(logical)); err != nil {
		return err
	}
	buf := make([]byte, decodeCopyBufferSize)
	var end uint64
	var frame [sparseFrameHeaderV1]byte
	for i := range count {
		if err := readSparseHeader(br, frame[:]); err != nil {
			return err
		}
		off := binary.LittleEndian.Uint64(frame[:8])
		length := binary.LittleEndian.Uint64(frame[8:])
		if length == 0 || off < end || off > logical || length > logical-off {
			return fmt.Errorf("%w: sparse frame %d (offset %d, length %d) is empty, out of order, or past the %d byte file", errIntegrity, i, off, length, logical)
		}
		n, err := io.CopyBuffer(io.NewOffsetWriter(dst, int64(off)), io.LimitReader(br, int64(length)), buf)
		if err != nil {
			return err
		}
		if n < int64(length) {
			return fmt.Errorf("%w: blob ends inside the %d byte sparse frame at %d", errIntegrity, length, off)
		}
		end = off + length
	}
	switch _, err := br.ReadByte(); {
	case err == nil:
		return fmt.Errorf("%w: blob holds bytes past its last sparse frame", errIntegrity)
	case err != io.EOF: // A wrapped EOF is a failed read, not the blob's end.
		return err
	}
	return nil
}

// readSparseHeader fills p from r, reporting a blob that ends first as an
// integrity failure. Only an unwrapped EOF means the blob ended: the Azure
// download body reports a dropped connection as ErrUnavailable wrapping an
// EOF, which must keep its code.
func readSparseHeader(r io.Reader, p []byte) error {
	_, err := io.ReadFull(r, p)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return fmt.Errorf("%w: blob ends inside a sparse snapshot header", errIntegrity)
	}
	return err
}
