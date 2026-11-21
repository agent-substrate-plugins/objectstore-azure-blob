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

//go:build !linux

package provider

import "os"

// dataExtents returns the whole file as one extent: this platform's holes are
// not looked up, so they upload as zeros.
func dataExtents(_ *os.File, size int64) ([]extent, error) {
	return wholeFile(size), nil
}
