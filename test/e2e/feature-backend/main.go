// feature-backend 为定向集成验证回显真实上游协议、路径与 Host,不自行压缩。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18085", "HTTP listen address")
	name := flag.String("name", "api", "backend identity")
	onlyH2C := flag.Bool("h2c", false, "serve HTTP/2 prior knowledge and reject HTTP/1 requests")
	flag.Parse()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if *onlyH2C && r.ProtoMajor != 2 {
			http.Error(w, "HTTP/2 required", http.StatusHTTPVersionNotSupported)
			return
		}
		padding := 2048
		if value := r.URL.Query().Get("padding"); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 || n > 65536 {
				http.Error(w, "invalid padding", http.StatusBadRequest)
				return
			}
			padding = n
		}
		body, err := json.Marshal(struct {
			Backend      string `json:"backend"`
			Protocol     string `json:"protocol"`
			Path         string `json:"path"`
			Query        string `json:"query"`
			Host         string `json:"host"`
			OriginalPath string `json:"original_path"`
			Padding      string `json:"padding"`
		}{*name, r.Proto, r.URL.Path, r.URL.RawQuery, r.Host, r.Header.Get("X-Envoy-Original-Path"), strings.Repeat("x", padding)})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// 显式长度让 Envoy 可以验证 min_content_length,不依赖分块响应行为。
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	})
	var serve http.Handler = handler
	if *onlyH2C {
		serve = h2c.NewHandler(handler, &http2.Server{})
	}
	s := &http.Server{Addr: *listen, Handler: serve, ReadHeaderTimeout: 5 * time.Second}
	fmt.Printf("feature-backend %s listening on %s (h2c-only=%t)\n", *name, *listen, *onlyH2C)
	log.Fatal(s.ListenAndServe())
}
