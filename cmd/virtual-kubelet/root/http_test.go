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

package root

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest/fake"
)

var _ = Describe("Metrics proxy routes", func() {
	const clusterID = "local-cluster-id"

	var (
		ctx        context.Context
		mux        *http.ServeMux
		calls      atomic.Int64
		failScrape atomic.Bool
		statusCode atomic.Int64
	)

	BeforeEach(func() {
		ctx = context.Background()
		calls.Store(0)
		failScrape.Store(false)
		statusCode.Store(http.StatusOK)

		cl := &fake.RESTClient{
			NegotiatedSerializer: clientscheme.Codecs.WithoutConversion(),
			GroupVersion:         schema.GroupVersion{Group: "metrics.liqo.io", Version: "v1beta1"},
			Client: fake.CreateHTTPClient(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				if failScrape.Load() {
					return nil, errors.New("scrape failure")
				}
				return &http.Response{
					StatusCode: int(statusCode.Load()),
					Header:     http.Header{"Content-Type": []string{"text/plain"}},
					Body:       io.NopCloser(strings.NewReader("metrics-payload")),
				}, nil
			}),
		}

		mux = http.NewServeMux()
		attachMetricsRoutes(ctx, mux, cl, clusterID, "", 15*time.Second)
	})

	doRequestWithEncoding := func(path, acceptEncoding string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
		if acceptEncoding != "" {
			req.Header.Set("Accept-Encoding", acceptEncoding)
		}
		mux.ServeHTTP(rec, req)
		return rec
	}

	doRequest := func(path string) *httptest.ResponseRecorder {
		return doRequestWithEncoding(path, "")
	}

	It("should proxy the remote metrics", func() {
		rec := doRequest("/metrics/resource")
		Expect(rec.Code).To(Equal(http.StatusOK))
		Expect(rec.Header().Get("Content-Encoding")).To(BeEmpty())
		Expect(rec.Header().Get("Content-Type")).To(Equal("text/plain; charset=utf-8"))
		Expect(rec.Body.String()).To(Equal("metrics-payload"))
	})

	It("should serve gzip-encoded metrics to clients accepting them", func() {
		rec := doRequestWithEncoding("/metrics/resource", "deflate, gzip")
		Expect(rec.Code).To(Equal(http.StatusOK))
		Expect(rec.Header().Get("Content-Encoding")).To(Equal("gzip"))
		Expect(rec.Header().Get("Content-Type")).To(Equal("text/plain; charset=utf-8"))

		gz, err := gzip.NewReader(rec.Body)
		Expect(err).ToNot(HaveOccurred())
		body, err := io.ReadAll(gz)
		Expect(err).ToNot(HaveOccurred())
		Expect(string(body)).To(Equal("metrics-payload"))
	})

	It("should serve plain metrics to clients explicitly refusing gzip", func() {
		rec := doRequestWithEncoding("/metrics/resource", "gzip;q=0")
		Expect(rec.Code).To(Equal(http.StatusOK))
		Expect(rec.Header().Get("Content-Encoding")).To(BeEmpty())
		Expect(rec.Body.String()).To(Equal("metrics-payload"))
	})

	It("should serve both encodings from the same cache entry", func() {
		doRequestWithEncoding("/metrics/resource", "gzip")
		Expect(doRequest("/metrics/resource").Body.String()).To(Equal("metrics-payload"))
		Expect(calls.Load()).To(BeNumerically("==", 1))
	})

	It("should fail if the remote scrape returns a non-successful status code", func() {
		statusCode.Store(http.StatusServiceUnavailable)
		Expect(doRequest("/metrics/resource").Code).To(Equal(http.StatusInternalServerError))
	})

	It("should serve subsequent requests for the same path from the cache", func() {
		doRequest("/metrics/resource")
		doRequest("/metrics/resource")
		Expect(calls.Load()).To(BeNumerically("==", 1))
	})

	It("should not alias requests differing in path", func() {
		doRequest("/metrics/resource")
		doRequest("/metrics/cadvisor")
		Expect(calls.Load()).To(BeNumerically("==", 2))
	})

	It("should cache failures briefly", func() {
		failScrape.Store(true)

		Expect(doRequest("/metrics/resource").Code).To(Equal(http.StatusInternalServerError))
		Expect(doRequest("/metrics/resource").Code).To(Equal(http.StatusInternalServerError))
		Expect(calls.Load()).To(BeNumerically("==", 1))
	})
})
