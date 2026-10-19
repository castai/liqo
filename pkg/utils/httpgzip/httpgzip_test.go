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

package httpgzip_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/liqotech/liqo/pkg/utils/httpgzip"
)

var _ = Describe("HTTP gzip helpers", func() {
	const payload = "# HELP metric1 help\nmetric1{pod=\"pod1\"} 1\n"

	decompress := func(data []byte) string {
		gz, err := gzip.NewReader(bytes.NewReader(data))
		Expect(err).ToNot(HaveOccurred())
		out, err := io.ReadAll(gz)
		Expect(err).ToNot(HaveOccurred())
		return string(out)
	}

	compressed := func(data string) []byte {
		out, err := httpgzip.Compress(strings.NewReader(data))
		Expect(err).ToNot(HaveOccurred())
		return out
	}

	Describe("Compress and CompressWith", func() {
		It("should compress the body read", func() {
			Expect(decompress(compressed(payload))).To(Equal(payload))
		})

		It("should compress the data written", func() {
			out, err := httpgzip.CompressWith(func(w io.Writer) error {
				_, err := io.WriteString(w, payload)
				return err
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(decompress(out)).To(Equal(payload))
		})

		It("should propagate the write errors", func() {
			_, err := httpgzip.CompressWith(func(io.Writer) error { return io.ErrUnexpectedEOF })
			Expect(err).To(MatchError(io.ErrUnexpectedEOF))
		})
	})

	Describe("EnsureCompressed", func() {
		It("should compress a plain body", func() {
			out, err := httpgzip.EnsureCompressed(strings.NewReader(payload))
			Expect(err).ToNot(HaveOccurred())
			Expect(decompress(out)).To(Equal(payload))
		})

		It("should return an already compressed body as-is", func() {
			original := compressed(payload)
			out, err := httpgzip.EnsureCompressed(bytes.NewReader(original))
			Expect(err).ToNot(HaveOccurred())
			Expect(out).To(Equal(original))
		})

		It("should handle an empty body", func() {
			out, err := httpgzip.EnsureCompressed(strings.NewReader(""))
			Expect(err).ToNot(HaveOccurred())
			Expect(decompress(out)).To(BeEmpty())
		})
	})

	DescribeTable("AcceptsGzip",
		func(acceptEncoding string, expected bool) {
			req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
			if acceptEncoding != "" {
				req.Header.Set("Accept-Encoding", acceptEncoding)
			}
			Expect(httpgzip.AcceptsGzip(req)).To(Equal(expected))
		},
		Entry("no header", "", false),
		Entry("gzip only", "gzip", true),
		Entry("gzip among others", "deflate, br, gzip", true),
		Entry("gzip with weight", "gzip;q=0.5", true),
		Entry("gzip explicitly refused", "gzip;q=0", false),
		Entry("gzip explicitly refused (decimal)", "gzip; q=0.0", false),
		Entry("other encodings only", "deflate, br", false),
	)

	Describe("Write", func() {
		write := func(acceptEncoding string) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
			if acceptEncoding != "" {
				req.Header.Set("Accept-Encoding", acceptEncoding)
			}
			Expect(httpgzip.Write(rec, req, compressed(payload), "text/plain")).To(Succeed())
			return rec
		}

		It("should write the body as-is to clients accepting gzip", func() {
			rec := write("gzip")
			Expect(rec.Code).To(Equal(http.StatusOK))
			Expect(rec.Header().Get("Content-Encoding")).To(Equal("gzip"))
			Expect(rec.Header().Get("Content-Type")).To(Equal("text/plain"))
			Expect(rec.Header().Get("Vary")).To(Equal("Accept-Encoding"))
			Expect(decompress(rec.Body.Bytes())).To(Equal(payload))
		})

		It("should write the body decompressed to the other clients", func() {
			rec := write("")
			Expect(rec.Code).To(Equal(http.StatusOK))
			Expect(rec.Header().Get("Content-Encoding")).To(BeEmpty())
			Expect(rec.Header().Get("Content-Type")).To(Equal("text/plain"))
			Expect(rec.Body.String()).To(Equal(payload))
		})
	})
})
