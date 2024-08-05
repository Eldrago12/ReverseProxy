package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"net/http/httptest"
	"runtime"

	"github.com/go-redis/redis/v8"
)

var ctx = context.Background()
var cacheMutex sync.Mutex
var maxClients = 10
var semaphore = make(chan struct{}, maxClients)

func newRedisClient() *redis.Client {
	rdb := redis.NewClient(&redis.Options{
		Addr: "gcp-managed-redis-url:6379",
	})
	return rdb
}

func cacheResponse(rdb *redis.Client, key string, response []byte, expiration time.Duration) {
	cacheMutex.Lock()
	defer cacheMutex.Unlock()

	err := rdb.Set(ctx, key, response, expiration).Err()
	if err != nil {
		log.Printf("Failed to set cache: %v", err)
	}
}

func getCachedResponse(rdb *redis.Client, key string) ([]byte, error) {
	cacheMutex.Lock()
	defer cacheMutex.Unlock()

	val, err := rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		return nil, nil // Cache miss
	} else if err != nil {
		return nil, err
	}
	return []byte(val), nil
}

func reverseProxyHandler(rdb *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		semaphore <- struct{}{}        // Acquire
		defer func() { <-semaphore }() // Release

		targetURLStr := r.Header.Get("X-Target-URL")
		if targetURLStr == "" {
			http.Error(w, "X-Target-URL header is required", http.StatusBadRequest)
			return
		}

		targetURL, err := url.Parse(targetURLStr)
		if err != nil {
			http.Error(w, "Invalid X-Target-URL header", http.StatusBadRequest)
			return
		}

		cacheKey := fmt.Sprintf("proxy:%s", r.URL.String())
		cachedResponse, err := getCachedResponse(rdb, cacheKey)
		if err == nil && cachedResponse != nil {
			w.Write(cachedResponse)
			return
		}

		proxy := httputil.NewSingleHostReverseProxy(targetURL)
		originalDirector := proxy.Director
		proxy.Director = func(req *http.Request) {
			originalDirector(req)
			req.Header = r.Header
			req.Header.Del("X-Real-IP")
			req.Header.Del("X-Forwarded-For")
			req.Header.Set("X-Forwarded-For", r.RemoteAddr)
		}

		respRecorder := httptest.NewRecorder()
		proxy.ServeHTTP(respRecorder, r)

		body, _ := io.ReadAll(respRecorder.Body)
		cacheResponse(rdb, cacheKey, body, 5*time.Minute)

		for k, v := range respRecorder.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(respRecorder.Code)
		w.Write(body)
	}
}

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	rdb := newRedisClient()
	defer rdb.Close()

	http.HandleFunc("/", reverseProxyHandler(rdb))

	log.Println("Starting proxy server on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
