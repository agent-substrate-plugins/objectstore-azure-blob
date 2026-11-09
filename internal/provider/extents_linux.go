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
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// dataExtents returns the populated extents of the first size bytes of file,
// found with SEEK_DATA and SEEK_HOLE. A kernel that cannot report holes gets
// the whole file as one extent.
func dataExtents(file *os.File, size int64) ([]extent, error) {
	conn, err := file.SyscallConn()
	if err != nil {
		return nil, err
	}
	var exts []extent
	var scanErr error
	if err := conn.Control(func(fd uintptr) { exts, scanErr = scanExtents(int(fd), size) }); err != nil {
		return nil, err
	}
	return exts, scanErr
}

func scanExtents(fd int, size int64) ([]extent, error) {
	var exts []extent
	for off := int64(0); off < size; {
		data, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break
		}
		if err != nil {
			if off == 0 && errors.Is(err, unix.EINVAL) {
				return wholeFile(size), nil
			}
			return nil, fmt.Errorf("finding data at offset %d: %w", off, err)
		}
		if data >= size {
			break
		}
		hole, err := unix.Seek(fd, data, unix.SEEK_HOLE)
		if err != nil {
			return nil, fmt.Errorf("finding the hole after offset %d: %w", data, err)
		}
		if hole <= data {
			return nil, fmt.Errorf("file system reported a hole at %d, not after the data at %d", hole, data)
		}
		hole = min(hole, size)
		exts = append(exts, extent{off: data, length: hole - data})
		off = hole
	}
	return exts, nil
}
