package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

type ProxyHandler struct {
	transport *http.Transport
	ids       chan uint
}

type proxyFunc func(*http.Request) (*url.URL, error)

func NewProxyHandler(proxy proxyFunc) ProxyHandler {
	return newProxyHandler(&http.Transport{Proxy: proxy})
}

func newProxyHandler(tr *http.Transport) ProxyHandler {
	ids := make(chan uint)
	go func() {
		for id := uint(0); ; id++ {
			ids <- id
		}
	}()
	return ProxyHandler{tr, ids}
}

func (ph ProxyHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	ctx = context.WithValue(ctx, "id", <-ph.ids)
	req = req.WithContext(ctx)
	deleteRequestHeaders(req)
	if req.Method == http.MethodConnect {
		handleConnect(w, req, ph.transport.Proxy)
	} else {
		proxyRequest(w, req, ph.transport)
	}
}

func handleConnect(w http.ResponseWriter, req *http.Request, proxy proxyFunc) {
	h, ok := w.(http.Hijacker)
	if !ok {
		msg := fmt.Sprintf("Can't hijack connection to %v", req.Host)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}
	u, err := proxy(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var server net.Conn
	if u == nil {
		server = connectToServer(w, req)
	} else {
		server = connectViaProxy(w, req, u.Host)
	}
	if server == nil {
		return
	}
	defer server.Close()
	client, _, err := h.Hijack()
	if err != nil {
		// The response status has already been sent, so if hijacking
		// fails, we can't return an error status to the client.
		// Instead, log the error and finish up.
		log.Printf("[%d] Error hijacking connection: %s", req.Context().Value("id"), err)
		return
	}
	defer client.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go transfer(req.Context(), server, client, &wg)
	go transfer(req.Context(), client, server, &wg)
	wg.Wait()
}

func connectViaProxy(w http.ResponseWriter, req *http.Request, proxy string) net.Conn {
	// can't hijack the connection to server, so can't just replay request via a Transport
	// need to dial and manually write connect header and read response
	conn, err := net.Dial("tcp", proxy)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil
	}
	err = req.Write(conn)
	if err != nil {
		log.Printf("[%d] Error sending CONNECT request: %v", req.Context().Value("id"), err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil
	}
	rd := bufio.NewReader(conn)
	resp, err := http.ReadResponse(rd, req)
	// should we close the response body, or leave it so that the
	// connection stays open?
	// ...also, might need to check for any buffered data in the reader,
	// and write it to the connection before moving on
	if err != nil {
		conn.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil
	}
	copyResponseHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil
	}
	return conn
}

func connectToServer(w http.ResponseWriter, req *http.Request) net.Conn {
	// TODO: should probably put a timeout on this
	conn, err := net.Dial("tcp", req.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil
	}
	w.WriteHeader(http.StatusOK)
	return conn
}

func transfer(ctx context.Context, dst, src net.Conn, wg *sync.WaitGroup) {
	defer wg.Done()
	_, err := io.Copy(dst, src)
	if err != nil {
		log.Printf("[%d] Error copying: %v", ctx.Value("id"), err)
	}
}

func proxyRequest(w http.ResponseWriter, req *http.Request, transport http.RoundTripper) {
	resp, err := transport.RoundTrip(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	copyResponseHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	if err != nil {
		// The response status has already been sent, so if copying
		// fails, we can't return an error status to the client.
		// Instead, log the error.
		log.Printf("[%d] Error copying response body: %s", req.Context().Value("id"), err)
		return
	}
}

func deleteConnectionTokens(header http.Header) {
	// Remove any header field(s) with the same name as a connection token (see
	// https://tools.ietf.org/html/rfc2616#section-14.10)
	if values, ok := header["Connection"]; ok {
		for _, value := range values {
			if value == "close" {
				continue
			}
			tokens := strings.Split(value, ",")
			for _, token := range tokens {
				header.Del(strings.TrimSpace(token))
			}
		}
	}
}

func deleteRequestHeaders(req *http.Request) {
	// Delete hop-by-hop headers (see https://tools.ietf.org/html/rfc2616#section-13.5.1)
	deleteConnectionTokens(req.Header)
	req.Header.Del("Connection")
	req.Header.Del("Keep-Alive")
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("TE")
	req.Header.Del("Upgrade")
}

func copyResponseHeaders(w http.ResponseWriter, resp *http.Response) {
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	// Delete hop-by-hop headers (see https://tools.ietf.org/html/rfc2616#section-13.5.1)
	deleteConnectionTokens(w.Header())
	w.Header().Del("Connection")
	w.Header().Del("Keep-Alive")
	w.Header().Del("Proxy-Authenticate")
	w.Header().Del("Trailer")
	w.Header().Del("Transfer-Encoding")
	w.Header().Del("Upgrade")
}
