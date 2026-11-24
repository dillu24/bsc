// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func confirmStatusCode(t *testing.T, got, want int) {
	t.Helper()
	if got == want {
		return
	}
	if gotName := http.StatusText(got); len(gotName) > 0 {
		if wantName := http.StatusText(want); len(wantName) > 0 {
			t.Fatalf("response status code: got %d (%s), want %d (%s)", got, gotName, want, wantName)
		}
	}
	t.Fatalf("response status code: got %d, want %d", got, want)
}

func confirmRequestValidationCode(t *testing.T, method, contentType, body string, expectedStatusCode int) {
	t.Helper()

	s := NewServer()
	request := httptest.NewRequest(method, "http://url.com", strings.NewReader(body))
	if len(contentType) > 0 {
		request.Header.Set("Content-Type", contentType)
	}
	code, err := s.validateRequest(request)
	if code == 0 {
		if err != nil {
			t.Errorf("validation: got error %v, expected nil", err)
		}
	} else if err == nil {
		t.Errorf("validation: code %d: got nil, expected error", code)
	}
	confirmStatusCode(t, code, expectedStatusCode)
}

func TestHTTPErrorResponseWithDelete(t *testing.T) {
	t.Parallel()

	confirmRequestValidationCode(t, http.MethodDelete, contentType, "", http.StatusMethodNotAllowed)
}

func TestHTTPErrorResponseWithPut(t *testing.T) {
	t.Parallel()

	confirmRequestValidationCode(t, http.MethodPut, contentType, "", http.StatusMethodNotAllowed)
}

func TestHTTPErrorResponseWithMaxContentLength(t *testing.T) {
	t.Parallel()

	body := make([]rune, defaultBodyLimit+1)
	confirmRequestValidationCode(t,
		http.MethodPost, contentType, string(body), http.StatusRequestEntityTooLarge)
}

func TestHTTPErrorResponseWithEmptyContentType(t *testing.T) {
	t.Parallel()

	confirmRequestValidationCode(t, http.MethodPost, "", "", http.StatusUnsupportedMediaType)
}

func TestHTTPErrorResponseWithValidRequest(t *testing.T) {
	t.Parallel()

	confirmRequestValidationCode(t, http.MethodPost, contentType, "", 0)
}

func confirmHTTPRequestYieldsStatusCode(t *testing.T, method, contentType, body string, expectedStatusCode int) {
	t.Helper()
	s := Server{}
	ts := httptest.NewServer(&s)
	defer ts.Close()

	request, err := http.NewRequest(method, ts.URL, strings.NewReader(body))
	if err != nil {
		t.Fatalf("failed to create a valid HTTP request: %v", err)
	}
	if len(contentType) > 0 {
		request.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	cleanlyCloseBody(resp.Body)
	confirmStatusCode(t, resp.StatusCode, expectedStatusCode)
}

func TestHTTPResponseWithEmptyGet(t *testing.T) {
	t.Parallel()

	confirmHTTPRequestYieldsStatusCode(t, http.MethodGet, "", "", http.StatusOK)
}

// This checks that maxRequestContentLength is not applied to the response of a request.
func TestHTTPRespBodyUnlimited(t *testing.T) {
	t.Parallel()

	const respLength = defaultBodyLimit * 3

	s := NewServer()
	defer s.Stop()
	s.RegisterName("test", largeRespService{respLength})
	ts := httptest.NewServer(s)
	defer ts.Close()

	c, err := DialHTTP(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var r string
	if err := c.Call(&r, "test_largeResp"); err != nil {
		t.Fatal(err)
	}
	if len(r) != respLength {
		t.Fatalf("response has wrong length %d, want %d", len(r), respLength)
	}
}

// Tests that an HTTP error results in an HTTPError instance
// being returned with the expected attributes.
func TestHTTPErrorResponse(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "error has occurred!", http.StatusTeapot)
	}))
	defer ts.Close()

	c, err := DialHTTP(ts.URL)
	if err != nil {
		t.Fatal(err)
	}

	var r string
	err = c.Call(&r, "test_method")
	if err == nil {
		t.Fatal("error was expected")
	}

	httpErr, ok := err.(HTTPError)
	if !ok {
		t.Fatalf("unexpected error type %T", err)
	}

	if httpErr.StatusCode != http.StatusTeapot {
		t.Error("unexpected status code", httpErr.StatusCode)
	}
	if httpErr.Status != "418 I'm a teapot" {
		t.Error("unexpected status text", httpErr.Status)
	}
	if body := string(httpErr.Body); body != "error has occurred!\n" {
		t.Error("unexpected body", body)
	}

	if errMsg := httpErr.Error(); errMsg != "418 I'm a teapot: error has occurred!\n" {
		t.Error("unexpected error message", errMsg)
	}
}

func TestHTTPPeerInfo(t *testing.T) {
	t.Parallel()

	s := newTestServer()
	defer s.Stop()
	ts := httptest.NewServer(s)
	defer ts.Close()

	c, err := Dial(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	c.SetHeader("user-agent", "ua-testing")
	c.SetHeader("origin", "origin.example.com")

	// Request peer information.
	var info PeerInfo
	if err := c.Call(&info, "test_peerInfo"); err != nil {
		t.Fatal(err)
	}

	if info.RemoteAddr == "" {
		t.Error("RemoteAddr not set")
	}
	if info.Transport != "http" {
		t.Errorf("wrong Transport %q", info.Transport)
	}
	if info.HTTP.Version != "HTTP/1.1" {
		t.Errorf("wrong HTTP.Version %q", info.HTTP.Version)
	}
	if info.HTTP.UserAgent != "ua-testing" {
		t.Errorf("wrong HTTP.UserAgent %q", info.HTTP.UserAgent)
	}
	if info.HTTP.Origin != "origin.example.com" {
		t.Errorf("wrong HTTP.Origin %q", info.HTTP.UserAgent)
	}
}

func TestNewContextWithHeaders(t *testing.T) {
	t.Parallel()

	expectedHeaders := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		for i := 0; i < expectedHeaders; i++ {
			key, want := fmt.Sprintf("key-%d", i), fmt.Sprintf("val-%d", i)
			if have := request.Header.Get(key); have != want {
				t.Errorf("wrong request headers for %s, want: %s, have: %s", key, want, have)
			}
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()

	client, err := Dial(server.URL)
	if err != nil {
		t.Fatalf("failed to dial: %s", err)
	}
	defer client.Close()

	newHdr := func(k, v string) http.Header {
		header := http.Header{}
		header.Set(k, v)
		return header
	}
	ctx1 := NewContextWithHeaders(context.Background(), newHdr("key-0", "val-0"))
	ctx2 := NewContextWithHeaders(ctx1, newHdr("key-1", "val-1"))
	ctx3 := NewContextWithHeaders(ctx2, newHdr("key-2", "val-2"))

	expectedHeaders = 3
	if err := client.CallContext(ctx3, nil, "test"); err != ErrNoResult {
		t.Error("call failed", err)
	}

	expectedHeaders = 2
	if err := client.CallContext(ctx2, nil, "test"); err != ErrNoResult {
		t.Error("call failed:", err)
	}
}

// readWithReference reads body with the plain jsonCodec, the way HTTP requests were
// read before httpServerCodec, and returns the result as text for comparison.
func readWithReference(body io.Reader) string {
	dec := json.NewDecoder(body)
	dec.UseNumber()
	conn := &httpServerConn{Reader: body, Writer: io.Discard, r: httptest.NewRequest(http.MethodPost, "/", nil)}
	msgs, batch, err := NewFuncCodec(conn, nil, dec.Decode).readBatch()
	return describeRead(msgs, batch, err)
}

func describeRead(msgs []*jsonrpcMessage, batch bool, err error) string {
	enc, _ := json.Marshal(msgs)
	return fmt.Sprintf("msgs=%s batch=%v err=%v", enc, batch, err)
}

// The HTTP codec must read every body exactly as the plain codec does: the same
// messages, the same batch flag and the same error.
func TestHTTPServerCodecReadsLikeJSONCodec(t *testing.T) {
	bodies := []string{
		`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x01","data":"0xabcd"},"latest"]}`,
		` {"jsonrpc":"2.0","id":"a","method":"x"} `,
		`{"jsonrpc":"2.0","id":1,"method":5}`,
		`{"jsonrpc":"2.0","id":1,"method":"x"} trailing`,
		`{"jsonrpc":"2.0","id":1,"method":"x"}{"id":2}`,
		`{"jsonrpc":"2.0","id":1,"method":"x","method":"y"}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":1,"message":"m","data":1.5}}`,
		`{"jsonrpc":"2.0","id":1,"method":"x"`,
		`[{"jsonrpc":"2.0","id":1,"method":"a"},{"jsonrpc":"2.0","id":2,"method":"b"}]`,
		`[]`, `[null]`, `null`, `{}`, `{"id":[1]}`, `{"params":"str"}`, `"str"`, `5`, `}`, ``, `   `,
	}
	for _, body := range bodies {
		for _, length := range []int64{int64(len(body)), -1} {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			req.ContentLength = length
			codec := NewServer().newHTTPServerConn(req, httptest.NewRecorder())
			got := describeRead(codec.readBatch())
			if want := readWithReference(strings.NewReader(body)); got != want {
				t.Errorf("body %q, length %d:\n got %s\nwant %s", body, length, got, want)
			}
		}
	}
}

// A read error must reach the caller as before, also when part of the body arrived.
func TestHTTPServerCodecReadErrorLikeJSONCodec(t *testing.T) {
	failure := errors.New("connection reset")
	for _, prefix := range []string{``, `{"jsonrpc":"2.0","id":1,`, `{"jsonrpc":"2.0","id":1,"method":"x"}`} {
		body := func() io.Reader { return io.MultiReader(strings.NewReader(prefix), errorReader{failure}) }
		req := httptest.NewRequest(http.MethodPost, "/", body())
		req.ContentLength = int64(len(prefix) + 10)
		codec := NewServer().newHTTPServerConn(req, httptest.NewRecorder())
		got := describeRead(codec.readBatch())
		if want := readWithReference(body()); got != want {
			t.Errorf("prefix %q:\n got %s\nwant %s", prefix, got, want)
		}
	}
}

// A success reply written directly must be byte for byte what json.Encoder writes, and
// replies the encoder would change must not be written directly.
func TestHTTPDirectResultWriteLikeEncoder(t *testing.T) {
	results := []any{"0xabcd", "plain", "<tag>&", "line\u2028sep", map[string]int{"a": 1}, []int{1, 2}, nil, 7}
	ids := []string{"1", "-5", "1.5e3", `"abc"`, `"\u0041"`, `"<x>"`, `"a b"`, "null"}
	direct := 0
	for _, id := range ids {
		for _, res := range results {
			msg := (&jsonrpcMessage{ID: json.RawMessage(id)}).response(res)
			if !canWriteResultDirectly(msg) {
				continue
			}
			direct++
			var want, got bytes.Buffer
			json.NewEncoder(&want).Encode(msg)
			if err := writeResultDirectly(&got, msg); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want.Bytes()) {
				t.Errorf("id %s result %v:\n got %q\nwant %q", id, res, got.String(), want.String())
			}
		}
	}
	if direct == 0 {
		t.Fatal("no reply took the direct path")
	}
	for _, id := range []string{`"<x>"`, `"a b"`} {
		if canWriteResultDirectly((&jsonrpcMessage{ID: json.RawMessage(id)}).response("0x")) {
			t.Errorf("id %s must go through the encoder", id)
		}
	}
	errReply := (&jsonrpcMessage{ID: json.RawMessage("1")}).errorResponse(errors.New("boom"))
	if canWriteResultDirectly(errReply) {
		t.Error("an error reply must go through the encoder")
	}
}
