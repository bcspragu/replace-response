// Copyright 2020 Matthew Holt
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

// Package replaceresponse registers a Caddy HTTP handler module that
// performs replacements on response bodies.
package replaceresponse

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/icholy/replace"
	"golang.org/x/text/transform"
)

var randReplace *rand.Rand

func init() {
	caddy.RegisterModule(Handler{})
	// Generated from random.org, because why not
	var seed2 uint64 = 0x845a6f90b949a040

	// We probably lose a bit of entropy on the int64 -> uint64, but this shouldn't
	// be used for cryptographically sensitive purposes anyway, for many reasons, so
	// please don't.
	randReplace = rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), seed2))
}

// Handler manipulates response bodies by performing
// substring or regex replacements.
type Handler struct {
	// The list of replacements to make on the response body.
	Replacements []*Replacement `json:"replacements,omitempty"`

	// If true, perform replacements in a streaming fashion.
	// This is more memory-efficient but can remove the
	// Content-Length header since knowing the correct length
	// is impossible without buffering, and getting it wrong
	// can break HTTP/2 streams.
	Stream bool `json:"stream,omitempty"`

	// Only run replacements on responses that match against this ResponseMmatcher.
	Matcher *caddyhttp.ResponseMatcher `json:"match,omitempty"`

	transformerPool *sync.Pool

	repl *caddy.Replacer
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.replace_response",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision implements caddy.Provisioner.
func (h *Handler) Provision(ctx caddy.Context) error {
	if len(h.Replacements) == 0 {
		return fmt.Errorf("no replacements configured")
	}

	// prepare each replacement
	for i, repl := range h.Replacements {
		if repl.Search == "" && repl.SearchRegexp == "" {
			return fmt.Errorf("replacement %d: no search or search_regexp configured", i)
		}
		if repl.Search != "" && repl.SearchRegexp != "" {
			return fmt.Errorf("replacement %d: cannot specify both search and search_regexp in same replacement", i)
		}
		if repl.SearchRegexp != "" {
			re, err := regexp.Compile(repl.SearchRegexp)
			if err != nil {
				return fmt.Errorf("replacement %d: %v", i, err)
			}
			repl.re = re
		}
	}

	placeholderRepl := caddy.NewReplacer()


	h.transformerPool = &sync.Pool{
		New: func() interface{} {
			transforms := make([]transform.Transformer, len(h.Replacements))
			for i, repl := range h.Replacements {
				finalReplace := placeholderRepl.ReplaceKnown(repl.Replaces[randReplace.IntN(len(repl.Replaces))], "")

				if repl.re != nil {
					tr := replace.RegexpIndexFunc(repl.re, func(src []byte, index []int) []byte {
						template := h.repl.ReplaceKnown(finalReplace, "")
						return repl.re.Expand(nil, []byte(template), src, index)
					})

					// See: https://github.com/icholy/replace/issues/5#issuecomment-949757616
					tr.MaxMatchSize = 2048
					transforms[i] = tr
				} else {
					finalSearch := placeholderRepl.ReplaceKnown(repl.Search, "")
					transforms[i] = replace.String(
						h.repl.ReplaceKnown(finalSearch, ""),
						h.repl.ReplaceKnown(finalReplace, ""),
					)
				}
			}
			return transform.Chain(transforms...)
		},
	}

	return nil
}

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {

	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	h.repl = repl

	tr := h.transformerPool.Get().(transform.Transformer)
	tr.Reset()
	defer h.transformerPool.Put(tr)

	if h.Stream {
		// don't buffer response body, perform streaming replacement
		fw := &replaceWriter{
			ResponseWriterWrapper: &caddyhttp.ResponseWriterWrapper{ResponseWriter: w},
			tr:                    tr,
			handler:               h,
		}
		err := next.ServeHTTP(fw, r)
		if err != nil {
			return err
		}
		// only close if there is no error; see PR #21
		// as of May 2023, Close() only flushes remaining bytes, but
		// this ends up calling WriteHeader() even if we don't want that
		fw.Close()
		return nil
	}

	// get a buffer to hold the response body
	respBuf := bufPool.Get().(*bytes.Buffer)
	respBuf.Reset()
	defer bufPool.Put(respBuf)

	// set up the response recorder
	shouldBuf := func(status int, headers http.Header) bool {
		if h.Matcher != nil {
			return h.Matcher.Match(status, headers)
		} else {
			// Always replace if no matcher is specified
			return true
		}
	}
	rec := caddyhttp.NewResponseRecorder(w, respBuf, shouldBuf)

	// collect the response from upstream
	err := next.ServeHTTP(rec, r)
	if err != nil {
		return err
	}
	if !rec.Buffered() {
		return nil // Skipped, no need to replace
	}

	// TODO: could potentially use transform.Append here with a pooled byte slice as buffer?
	result, _, err := transform.Bytes(tr, rec.Buffer().Bytes())
	if err != nil {
		return err
	}

	// make sure length is correct, otherwise bad things can happen
	if w.Header().Get("Content-Length") != "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(result)))
	}

	if status := rec.Status(); status > 0 {
		w.WriteHeader(status)
	}
	w.Write(result)

	return nil
}

// Replacement is either a substring or regular expression replacement
// to perform; precisely one must be specified, not both.
type Replacement struct {
	// A substring to search for. Mutually exclusive with search_regexp.
	Search string `json:"search,omitempty"`

	// A regular expression to search for. Mutually exclusive with search.
	SearchRegexp string `json:"search_regexp,omitempty"`

	// The replacement strings/values. Required.
	Replaces []string `json:"replace"`

	re *regexp.Regexp
}

// replaceWriter is used for streaming response body replacement. It
// ensures the Content-Length header is removed and writes to tw,
// which should be a transform writer that performs replacements.
type replaceWriter struct {
	*caddyhttp.ResponseWriterWrapper
	wroteHeader bool
	tw          io.WriteCloser
	tr          transform.Transformer
	handler     *Handler
}

func (fw *replaceWriter) WriteHeader(status int) {
	if fw.wroteHeader {
		return
	}
	fw.wroteHeader = true

	if fw.handler.Matcher == nil || fw.handler.Matcher.Match(status, fw.ResponseWriterWrapper.Header()) {
		// we don't know the length after replacements since
		// we're not buffering it all to find out
		fw.Header().Del("Content-Length")
		fw.tw = transform.NewWriter(fw.ResponseWriterWrapper, fw.tr)
	}

	fw.ResponseWriterWrapper.WriteHeader(status)
}

func (fw *replaceWriter) Write(d []byte) (int, error) {
	if !fw.wroteHeader {
		fw.WriteHeader(http.StatusOK)
	}

	if fw.tw != nil {
		return fw.tw.Write(d)
	} else {
		return fw.ResponseWriterWrapper.Write(d)
	}
}

func (fw *replaceWriter) Close() error {
	if fw.tw != nil {
		// Close if we have a transform writer, the underlying one does not need to be closed.
		return fw.tw.Close()
	}
	return nil
}

var bufPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
	_ caddyfile.Unmarshaler       = (*Handler)(nil)

	_ http.ResponseWriter = (*replaceWriter)(nil)
)
