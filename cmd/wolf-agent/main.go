package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	"games-on-whales.github.io/direwolf/pkg/controllers"
	"games-on-whales.github.io/direwolf/pkg/util"
	"games-on-whales.github.io/direwolf/pkg/wolfapi"
	"k8s.io/klog/v2"
)

func main() {
	appContext, appCancel := context.WithCancel(context.Background())
	defer appCancel()

	serverCertPath := flag.String("tls-cert", "server.crt", "Path to server cert")
	serverKeyPath := flag.String("tls-key", "server.key", "Path to server key")
	serverPort := flag.Int("port", 443, "Port to listen on")
	wolfSocketPath := flag.String("socket", "/var/run/wolf.sock", "Path to wolf.sock")
	klog.InitFlags(nil)
	flag.Parse()

	klog.Info("Starting wolf-agent")
	klog.Info("TLS Cert:", *serverCertPath)
	klog.Info("TLS Key:", *serverKeyPath)
	klog.Info("Port:", *serverPort)
	klog.Info("Wolf Socket:", *wolfSocketPath)
	client := UnixHTTPClient(*wolfSocketPath)

	// Generate self-signed certificate and key
	cert, err := util.LoadCertificates(*serverCertPath, *serverKeyPath)
	if err != nil {
		klog.Fatal("Failed to load certificates:", err)
	}

	// Start a thread to watch for the wolf.sock to appear
	var ready atomic.Bool
	go func() {
		for {
			// Check socket exists
			if info, err := os.Stat(*wolfSocketPath); err == nil && info != nil && info.Mode()&os.ModeSocket != 0 {
				conn, err := net.Dial("unix", *wolfSocketPath)
				if err == nil {
					defer conn.Close()
					klog.Info("wolf.sock is ready")

					// Call out to the proxy which handles chunked encoding
					// properly. There may be a way to use the SSE client without
					// it, but found this easier.
					wolfClient := wolfapi.NewClient(
						fmt.Sprintf("https://localhost:%d", *serverPort),
						&http.Client{
							Transport: &http.Transport{
								TLSClientConfig: &tls.Config{
									InsecureSkipVerify: true,
								},
							},
						},
					)

					agentController := controllers.NewAgent(
						wolfClient,
					)

					go agentController.Run(appContext)

					// Set ready to true
					// This will allow the /readyz endpoint to return 200 OK
					// and the server to start accepting connections
					ready.Store(true)
					return
				}
				klog.Warningf("Waiting for wolf.sock to accept connections: %v\n", err)
			} else if err == nil && info.Mode()&os.ModeSocket == 0 {
				klog.Fatal("wolf.sock is not a socket", info.Mode())

			} else {
				klog.Info("Waiting for wolf.sock to appear...", err)
			}
			<-time.After(200 * time.Millisecond)
		}
	}()

	// Spin up HTTPS server with self-signed certificate to service the wolf.sock
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		klog.Info("Received request:", r.Method, r.URL.Path)
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		//!TODO: Use kubernetes metric.Filter or something to implement RBAC
		// authorization against the bearer token
		// Proxy the request to the wolf.sock
		url, err := url.JoinPath("http://", "wolf.sock", r.URL.Path)
		if err != nil {
			klog.ErrorS(err, "Failed to join URL")
			http.Error(w, fmt.Sprintf("Failed to join URL: %v", err), http.StatusInternalServerError)
			return
		}
		request, err := http.NewRequest(r.Method, url, r.Body)
		request.Proto = r.Proto
		request.ProtoMajor = r.ProtoMajor
		request.ProtoMinor = r.ProtoMinor
		request.TransferEncoding = r.TransferEncoding
		request.ContentLength = r.ContentLength
		if err != nil {
			klog.ErrorS(err, "Failed to create proxy request")
			http.Error(w, fmt.Sprintf("Failed to create proxy request: %v", err), http.StatusInternalServerError)
			return
		}
		request.Header = r.Header.Clone()

		// Send the request to the wolf.sock
		klog.Info("Sending request to wolf.sock:", request.Method, request.URL.Path)
		response, err := client.Do(request.WithContext(r.Context()))
		if err != nil {
			klog.ErrorS(err, "Failed to send request to wolf.sock")
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer response.Body.Close()

		// Write the response back to the client
		for key, values := range response.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		flusher, ok := w.(http.Flusher)
		if !ok {
			klog.Error("Flushing not supported! Aborting writing response")
			return
		}

		// Stream response body manually. io.Copy doesn't eagerly flush
		// which breaks SSE stream.
		buf := make([]byte, 4096)
		for {
			n, err := response.Body.Read(buf)
			if n > 0 {
				_, writeErr := w.Write(buf[:n])
				if writeErr != nil {
					klog.Info("Client connection closed")
					return
				}
				flusher.Flush() // Ensure immediate delivery
			}
			if err != nil {
				if err == io.EOF {
					break
				}
				klog.ErrorS(err, "Error reading from backend")
				return
			}
		}
		klog.InfoS("Request completed", "statusCode", response.StatusCode)
	})

	// Start HTTPS server
	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", *serverPort),
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
		},
	}

	klog.Infof("Listening on port %d\n", *serverPort)
	err = server.ListenAndServeTLS("", "")
	if err != nil {
		klog.Fatal("Failed to start server:", err)
	}
}

func UnixHTTPClient(sockAddr string) http.Client {
	return http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.Dial("unix", sockAddr)
			},
		},
	}
}
