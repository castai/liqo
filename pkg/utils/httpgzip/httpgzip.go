// Copyright 2019-2026 The Liqo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package httpgzip

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// gzipMagic is the header identifying gzip-compressed data (RFC 1952).
var gzipMagic = []byte{0x1f, 0x8b}

// maxDecompressedSize bounds the size of the bodies decompressed for the clients not accepting gzip, as the
// compressed ones may come from a third party (i.e., a remote cluster). It is way larger than any expected
// metrics payload (a few MBs), while preventing decompression bombs from streaming unbounded responses.
const maxDecompressedSize = 256 << 20

// writers pools the gzip writers, as each one allocates a large internal state.
var writers = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

// Compress reads the given body and returns it gzip-compressed. The body is compressed while read,
// so that it is never entirely materialized uncompressed in memory.
func Compress(body io.Reader) ([]byte, error) {
	return CompressWith(func(w io.Writer) error {
		_, err := io.Copy(w, body)
		return err
	})
}

// CompressWith returns the data written by the given function gzip-compressed. The data is compressed
// while written, so that it is never entirely materialized uncompressed in memory.
func CompressWith(write func(io.Writer) error) ([]byte, error) {
	var buf bytes.Buffer
	gz := writers.Get().(*gzip.Writer)
	defer writers.Put(gz)
	gz.Reset(&buf)

	if err := write(gz); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	// Clone the result, to avoid retaining the spare capacity of the buffer while cached.
	return bytes.Clone(buf.Bytes()), nil
}

// EnsureCompressed reads the given body and returns it gzip-compressed: as-is if already gzip-compressed (as
// detected from its magic header), and compressing it otherwise. It allows requesting gzip-encoded responses
// to servers which may or may not honor the request (e.g., depending on their version).
func EnsureCompressed(body io.Reader) ([]byte, error) {
	reader := bufio.NewReader(body)
	if header, _ := reader.Peek(len(gzipMagic)); !bytes.Equal(header, gzipMagic) {
		return Compress(reader)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(reader); err != nil {
		return nil, err
	}
	return bytes.Clone(buf.Bytes()), nil
}

// Write writes the given gzip-compressed body with the given content type and a 200 status code: as-is if
// the client accepts gzip-encoded responses, and decompressing it on the fly otherwise.
func Write(w http.ResponseWriter, r *http.Request, gzipped []byte, contentType string) error {
	// The content type is set explicitly, as it would be otherwise sniffed from the (compressed) body.
	w.Header().Set("Content-Type", contentType)
	w.Header().Add("Vary", "Accept-Encoding")

	if AcceptsGzip(r) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(gzipped)))
		w.WriteHeader(http.StatusOK)
		_, err := w.Write(gzipped)
		return err
	}

	gz, err := gzip.NewReader(bytes.NewReader(gzipped))
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	if _, err = io.CopyN(w, gz, maxDecompressedSize); errors.Is(err, io.EOF) {
		return nil
	} else if err == nil {
		return fmt.Errorf("decompressed body exceeds the maximum size of %d bytes", maxDecompressedSize)
	}
	return err
}

// AcceptsGzip returns whether the client accepts gzip-encoded response bodies.
func AcceptsGzip(r *http.Request) bool {
	for _, value := range r.Header.Values("Accept-Encoding") {
		for _, item := range strings.Split(value, ",") {
			encoding, params, _ := strings.Cut(item, ";")
			if !strings.EqualFold(strings.TrimSpace(encoding), "gzip") {
				continue
			}
			// Honor an explicit refusal (i.e., "gzip;q=0").
			if qvalue, found := strings.CutPrefix(strings.TrimSpace(params), "q="); found {
				q, err := strconv.ParseFloat(qvalue, 64)
				return err == nil && q > 0
			}
			return true
		}
	}
	return false
}
