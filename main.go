package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/net/proxy"
)

var (
	currentSettings Settings
	settingsMutex   sync.RWMutex
)

type TorrentSession struct {
	Client   *torrent.Client
	Torrent  *torrent.Torrent
	Port     int
	LastUsed time.Time
	Magnet   string
	FileIdx  int
	mu       sync.Mutex
}

const sessionIdleTimeout = 4 * time.Hour

// magnetStore keeps magnet links in memory keyed by infohash so sessions
// can be recreated on demand when a stream request arrives for a session
// that was cleaned up or lost to a server restart. This is in-memory only —
// no disk persistence. Each movie request creates a fresh session.
var magnetStore sync.Map // map[string]string (infohash -> magnet)

type Settings struct {
	EnableProxy    bool   `json:"enableProxy"`
	ProxyURL       string `json:"proxyUrl"`
	EnableProwlarr bool   `json:"enableProwlarr"`
	ProwlarrHost   string `json:"prowlarrHost"`
	ProwlarrApiKey string `json:"prowlarrApiKey"`
	EnableJackett  bool   `json:"enableJackett"`
	JackettHost    string `json:"jackettHost"`
	JackettApiKey  string `json:"jackettApiKey"`
}

type ProxySettings struct {
	EnableProxy bool   `json:"enableProxy"`
	ProxyURL    string `json:"proxyUrl"`
}

type ProwlarrSettings struct {
	EnableProwlarr bool   `json:"enableProwlarr"`
	ProwlarrHost   string `json:"prowlarrHost"`
	ProwlarrApiKey string `json:"prowlarrApiKey"`
}

type JackettSettings struct {
	EnableJackett bool   `json:"enableJackett"`
	JackettHost   string `json:"jackettHost"`
	JackettApiKey string `json:"jackettApiKey"`
}

var (
	sessions  sync.Map
	usedPorts sync.Map
	portMutex sync.Mutex

)

// Helper function to format file sizes
func formatSize(sizeInBytes float64) string {
	if sizeInBytes < 1024 {
		return fmt.Sprintf("%.0f B", sizeInBytes)
	}

	sizeInKB := sizeInBytes / 1024
	if sizeInKB < 1024 {
		return fmt.Sprintf("%.2f KB", sizeInKB)
	}

	sizeInMB := sizeInKB / 1024
	if sizeInMB < 1024 {
		return fmt.Sprintf("%.2f MB", sizeInMB)
	}

	sizeInGB := sizeInMB / 1024
	return fmt.Sprintf("%.2f GB", sizeInGB)
}

var (
	proxyTransport = &http.Transport{
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConnsPerHost:   10,
	}
	proxyClient = &http.Client{
		Transport: proxyTransport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			for k, vv := range via[0].Header {
				if _, ok := req.Header[k]; !ok {
					req.Header[k] = vv
				}
			}
			return nil
		},
	}
)

func createSelectiveProxyClient() *http.Client {
	settingsMutex.RLock()
	defer settingsMutex.RUnlock()

	if !currentSettings.EnableProxy {
		return &http.Client{Timeout: 30 * time.Second}
	}
	// Reconfigure proxyTransport's DialContext if URL changed:
	dialer, _ := createProxyDialer(currentSettings.ProxyURL)
	proxyTransport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialer.Dial(network, addr)
	}
	// Drop any old idle conns after reconfiguration:
	proxyTransport.CloseIdleConnections()

	return proxyClient
}

// Create a proxy dialer for SOCKS5
func createProxyDialer(proxyURL string) (proxy.Dialer, error) {
	proxyURLParsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse proxy URL: %v", err)
	}

	// Extract auth information
	auth := &proxy.Auth{}
	if proxyURLParsed.User != nil {
		auth.User = proxyURLParsed.User.Username()
		if password, ok := proxyURLParsed.User.Password(); ok {
			auth.Password = password
		}
	}

	// Create a SOCKS5 dialer
	return proxy.SOCKS5("tcp", proxyURLParsed.Host, auth, proxy.Direct)
}

// Implement a port allocation function to prevent conflicts
func getAvailablePort() int {
	portMutex.Lock()
	defer portMutex.Unlock()

	// Try up to 50 times to find an unused port
	for i := 0; i < 50; i++ {
		// Generate a random port in the high range
		port := 10000 + rand.Intn(50000)

		// Check if this port is already in use by our app
		if _, exists := usedPorts.Load(port); !exists {
			// Mark this port as used
			usedPorts.Store(port, true)
			return port
		}
	}

	// If we can't find an available port, return a very high random port
	// as a last resort
	return 60000 + rand.Intn(5000)
}

// Release a port when we're done with it
func releasePort(port int) {
	portMutex.Lock()
	defer portMutex.Unlock()
	usedPorts.Delete(port)
}

// Initialize the torrent client with proxy settings
func initTorrentWithProxy() (*torrent.Client, int, error) {
	settingsMutex.RLock()
	enableProxy := currentSettings.EnableProxy
	proxyURL := currentSettings.ProxyURL
	settingsMutex.RUnlock()

	config := torrent.NewDefaultClientConfig()
	config.DefaultStorage = storage.NewFile("./torrent-data")
	port := getAvailablePort()
	config.ListenPort = port
	config.DisableIPv6 = true

	if enableProxy {
		log.Println("Creating torrent client with proxy...")
		os.Setenv("ALL_PROXY", proxyURL)
		os.Setenv("SOCKS_PROXY", proxyURL)
		os.Setenv("HTTP_PROXY", proxyURL)
		os.Setenv("HTTPS_PROXY", proxyURL)

		proxyDialer, err := createProxyDialer(proxyURL)
		if err != nil {
			releasePort(port)
			return nil, port, fmt.Errorf("could not create proxy dialer: %v", err)
		}

		config.HTTPProxy = func(*http.Request) (*url.URL, error) {
			return url.Parse(proxyURL)
		}

		client, err := torrent.NewClient(config)
		if err != nil {
			releasePort(port)
			return nil, port, err
		}

		setValue(client, "dialerNetwork", func(ctx context.Context, network, addr string) (net.Conn, error) {
			return proxyDialer.Dial(network, addr)
		})

		return client, port, nil
	}

	log.Println("Creating torrent client without proxy...")
	os.Unsetenv("ALL_PROXY")
	os.Unsetenv("SOCKS_PROXY")
	os.Unsetenv("HTTP_PROXY")
	os.Unsetenv("HTTPS_PROXY")

	client, err := torrent.NewClient(config)
	if err != nil {
		releasePort(port)
		return nil, port, err
	}
	return client, port, nil
}

// Helper function to try to set a field value using reflection
// This is a bit hacky but might help override the client's dialer
func setValue(obj interface{}, fieldName string, value interface{}) {
	// This is a best-effort approach that may not work with all library versions
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Warning: Could not set %s field: %v", fieldName, r)
		}
	}()

	reflectValue := reflect.ValueOf(obj).Elem()
	field := reflectValue.FieldByName(fieldName)

	if field.IsValid() && field.CanSet() {
		field.Set(reflect.ValueOf(value))
		log.Printf("Successfully set %s to use proxy", fieldName)
	}
}

// Override system settings with our proxy
func init() {

	// check if settings.json exists
	if _, err := os.Stat("config/settings.json"); os.IsNotExist(err) {
		log.Println("settings.json not found, creating default settings")
		defaultSettings := Settings{
			EnableProxy:    false,
			ProxyURL:       "",
			EnableProwlarr: false,
			ProwlarrHost:   "",
			ProwlarrApiKey: "",
			EnableJackett:  false,
			JackettHost:    "",
			JackettApiKey:  "",
		}
		// Create the config directory if it doesn't exist
		if err := os.MkdirAll("config", 0755); err != nil {
			log.Fatalf("Failed to create config directory: %v", err)
		}
		settingsFile, err := os.Create("config/settings.json")
		if err != nil {
			log.Fatalf("Failed to create settings.json: %v", err)
		}
		defer settingsFile.Close()
		encoder := json.NewEncoder(settingsFile)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(defaultSettings); err != nil {
			log.Fatalf("Failed to encode default settings: %v", err)
		}
		log.Println("Default settings created in settings.json")
	}

	// Load settings from settings.json
	settingsFile, err := os.Open("config/settings.json")
	if err != nil {
		log.Fatalf("Failed to open settings.json: %v", err)
	}
	defer settingsFile.Close()

	var s Settings
	if err := json.NewDecoder(settingsFile).Decode(&s); err != nil {
		log.Fatalf("Failed to decode settings.json: %v", err)
	}

	settingsMutex.Lock()
	currentSettings = s
	settingsMutex.Unlock()
}

func main() {
	// Seed random number generator
	rand.Seed(time.Now().UnixNano())

	// Force proxy for all Go HTTP connections
	setGlobalProxy()

	// Set up endpoint handlers
	http.HandleFunc("/api/v1/torrent/add", recoverHandler(addTorrentHandler))
	http.HandleFunc("/api/v1/torrent/", recoverHandler(torrentHandler))
	http.HandleFunc("/api/v1/settings", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			settingsMutex.RLock()
			defer settingsMutex.RUnlock()
			respondWithJSON(w, http.StatusOK, currentSettings)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})
	http.HandleFunc("/api/v1/settings/proxy", saveProxySettingsHandler)
	http.HandleFunc("/api/v1/settings/prowlarr", saveProwlarrSettingsHandler)
	http.HandleFunc("/api/v1/settings/jackett", saveJackettSettingsHandler)
	http.HandleFunc("/api/v1/prowlarr/search", searchFromProwlarr)
	http.HandleFunc("/api/v1/jackett/search", searchFromJackett)
	http.HandleFunc("/api/v1/prowlarr/test", testProwlarrConnection)
	http.HandleFunc("/api/v1/jackett/test", testJackettConnection)
	http.HandleFunc("/api/v1/proxy/test", testProxyConnection)
	http.HandleFunc("/api/v1/torrent/convert", convertTorrentToMagnetHandler)
	http.HandleFunc("/process", recoverHandler(processHandler))
	http.HandleFunc("/api/v1/magnets", magnetsHandler)
	http.HandleFunc("/api/v1/meta", metaHandler)
	http.HandleFunc("/api/v1/keepalive", keepaliveHandler)
	http.HandleFunc("/movie/", contentPageHandler)
	http.HandleFunc("/series/", contentPageHandler)
	http.HandleFunc("/tv/", contentPageHandler)

	// Set up client file serving
	http.Handle("/", http.FileServer(http.Dir("./client")))
	http.HandleFunc("/client/", func(w http.ResponseWriter, r *http.Request) {
		http.StripPrefix("/client/", http.FileServer(http.Dir("./client"))).ServeHTTP(w, r)
	})
	http.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./client/favicon.ico")
	})

	go cleanupSessions()

	port := 7860

	addr := fmt.Sprintf(":%d", port)
	log.Printf("Attempting to start server on %s", addr)

	// Create channel to signal if server started successfully
	serverStarted := make(chan bool, 1)

	// Create a server with graceful shutdown
	server := &http.Server{
		Addr:    addr,
		Handler: nil, // Use the default ServeMux
	}

	// Start the server in a goroutine
	go func() {
		err := server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			log.Printf("Server failed on %s: %v", addr, err)
			serverStarted <- false
		}
	}()

	// Give the server a moment to start or fail
	select {
	case success := <-serverStarted:
		if !success {
			log.Printf("Server failed to start on %s", addr)
			return
		}
	case <-time.After(1 * time.Second):
		// No immediate error, assume it started successfully
		log.Printf("🚀 Server successfully started on %s", addr)

		// Create a simple message to display in the browser
		fmt.Printf("\n------------------------------------------------\n")
		fmt.Printf("✅ Server started! Open in your browser:\n")
		fmt.Printf("   http://localhost:%d\n", port)
		fmt.Printf("------------------------------------------------\n\n")

		// Block forever (the server is running in a goroutine)
		select {}
	}
}

// Set up global proxy for all Go HTTP calls
func setGlobalProxy() {
	settingsMutex.RLock()
	enableProxy := currentSettings.EnableProxy
	proxyURL := currentSettings.ProxyURL
	settingsMutex.RUnlock()

	if !enableProxy {
		log.Println("Proxy is disabled, not setting global HTTP proxy.")
		return
	}

	proxyDialer, err := createProxyDialer(proxyURL)
	if err != nil {
		log.Printf("Warning: Could not create proxy dialer: %v", err)
		return
	}

	httpTransport, ok := http.DefaultTransport.(*http.Transport)
	if ok {
		httpTransport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return proxyDialer.Dial(network, addr)
		}
		log.Printf("Successfully configured SOCKS5 proxy for all HTTP traffic: %s", proxyURL)
	} else {
		log.Println("⚠️ Warning: Could not override HTTP transport")
	}
}

// resolveMagnet resolves a raw input (magnet link or Prowlarr/Jackett http link)
// into a valid magnet: string. Returns the magnet and an error if it cannot be resolved.
func resolveMagnet(rawInput string) (string, error) {
	magnet := rawInput

	if strings.HasPrefix(rawInput, "http") {
		httpClient := createSelectiveProxyClient()
		httpClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}

		req, err := http.NewRequest("GET", rawInput, nil)
		if err != nil {
			return "", fmt.Errorf("invalid URL: %v", err)
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

		log.Printf("Following indexer URL: %s", rawInput)
		resp, err := httpClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("failed to download: %v", err)
		}
		defer resp.Body.Close()

		log.Printf("Got response: %d %s", resp.StatusCode, resp.Status)

		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			location := resp.Header.Get("Location")
			log.Printf("Found redirect to: %s", location)
			if strings.HasPrefix(location, "magnet:") {
				magnet = location
			} else {
				return "", fmt.Errorf("URL redirects to non-magnet content")
			}
		}
	}

	if magnet == "" || !strings.HasPrefix(magnet, "magnet:") {
		return "", fmt.Errorf("invalid magnet link")
	}
	return magnet, nil
}

// infoHashFromMagnet extracts the btih infohash from a magnet URI.
// Returns empty string if not found.
func infoHashFromMagnet(magnet string) string {
	u, err := url.Parse(magnet)
	if err != nil {
		return ""
	}
	xt := u.Query().Get("xt")
	const prefix = "urn:btih:"
	if strings.HasPrefix(strings.ToLower(xt), prefix) {
		return strings.ToLower(xt[len(prefix):])
	}
	return ""
}

// addTorrentByMagnet creates a torrent client, adds the magnet, waits for info,
// stores the session, and returns the session ID. If a session for the same
// infohash already exists and is still alive, it is reused — no new torrent
// client is created. The caller is responsible for error responses; this
// function only returns data/errors.
func addTorrentByMagnet(magnet string) (string, error) {
	// Store the magnet in the in-memory magnet store so this session can be
	// recreated later if it's cleaned up or the server restarts.
	if ih := infoHashFromMagnet(magnet); ih != "" {
		magnetStore.Store(ih, magnet)
	}

	client, port, err := initTorrentWithProxy()
	if err != nil {
		log.Printf("Client creation error: %v", err)
		return "", fmt.Errorf("failed to create client with proxy")
	}

	defer func() {
		if client != nil {
			releasePort(port)
			client.Close()
		}
	}()

	t, err := client.AddMagnet(magnet)
	if err != nil {
		return "", fmt.Errorf("invalid magnet url")
	}
	log.Printf("Torrent added: %s", t.InfoHash().HexString())

	// OPTIMIZED: Add comprehensive tracker list immediately for fastest peer discovery
	t.AddTrackers([][]string{
		{
			"udp://tracker.opentrackr.org:1337/announce",
			"http://tracker.opentrackr.org:1337/announce",
			"https://tracker.opentrackr.org:443/announce",
			"udp://tracker.torrent.eu.org:451/announce",
			"udp://tracker.moeking.me:6969/announce",
			"https://tracker.moeking.me:443/announce",
			"https://tracker.tamersunion.org:443/announce",
			"udp://tracker1.bt.moack.co.kr:80/announce",
			"http://tracker1.bt.moack.co.kr:80/announce",
			"udp://tracker.theoks.net:6969/announce",
			"udp://tracker.tiny-vps.com:6969/announce",
		},
		{
			"udp://tracker.openbittorrent.com:6969/announce",
			"http://tracker.openbittorrent.com:80/announce",
			"udp://opentracker.i2p.rocks:6969/announce",
			"udp://tracker.internetwarriors.net:1337/announce",
			"udp://tracker.leechers-paradise.org:6969/announce",
			"udp://tracker.coppersurfer.tk:6969/announce",
			"udp://tracker.cyberia.is:6969/announce",
			"udp://tracker.ds.is:6969/announce",
			"udp://tracker.dler.org:6969/announce",
			"udp://public.popcorn-tracker.org:6969/announce",
		},
		{
			"https://tracker.gbitt.info:443/announce",
			"https://tracker.imgoingto.icu:443/announce",
			"https://tracker.lelux.fi:443/announce",
			"https://tracker.loligirl.cn:443/announce",
			"http://tracker.bt4g.com:2095/announce",
			"http://tracker.electro-torrent.pl:80/announce",
			"http://tracker.files.fm:6969/announce",
			"http://tracker.ipv6tracker.ru:80/announce",
			"http://tracker3.ctix.cn:8080/announce",
			"http://tracker4.ctix.cn:8080/announce",
		},
		{
			"udp://retracker.lanta-net.ru:2710/announce",
			"udp://retracker.netbynet.ru:2710/announce",
			"udp://retracker.hotplug.ru:2710/announce",
			"udp://tracker.bittor.pw:1337/announce",
			"udp://tracker.justseed.it:1337/announce",
			"udp://tracker.open-internet.nl:6969/announce",
			"udp://tracker.zer0day.to:1337/announce",
			"udp://tracker.pirateparty.gr:6969/announce",
			"udp://tracker.novg.net:6969/announce",
			"udp://tracker.tvunderground.org.ru:3218/announce",
		},
		{
			"udp://tracker.altrosky.nl:6969/announce",
			"udp://tracker.bitsearch.to:1337/announce",
			"udp://tracker.dump.cl:6969/announce",
			"udp://tracker.torrentbay.to:6969/announce",
			"udp://tracker.zemoj.com:6969/announce",
			"udp://tracker.blackunicorn.xyz:6969/announce",
		},
	})
	log.Printf("Added optimized trackers for maximum peer discovery: %s", t.InfoHash().HexString())

	select {
	case <-t.GotInfo():
		log.Printf("Successfully got torrent info for %s", t.InfoHash().HexString())
	case <-time.After(45 * time.Second):
		return "", fmt.Errorf("timeout getting info - proxy might be blocking BitTorrent traffic")
	}

	sessionID := t.InfoHash().HexString()
	log.Printf("Creating new session with ID: %s", sessionID)
	fileIdx := pickVideoFileIndex(t)
	sessions.Store(sessionID, &TorrentSession{
		Client:   client,
		Torrent:  t,
		Port:     port,
		LastUsed: time.Now(),
		Magnet:   magnet,
		FileIdx:  fileIdx,
	})
	log.Printf("Successfully stored session: %s", sessionID)

	// Prevent the defer from closing the client now that it's stored
	client = nil
	return sessionID, nil
}

// pickVideoFileIndex returns the index of the largest playable video file in the
// torrent, or -1 if none is found.
func pickVideoFileIndex(t *torrent.Torrent) int {
	bestIndex := -1
	var bestSize int64
	for i, file := range t.Files() {
		ext := strings.ToLower(filepath.Ext(file.DisplayPath()))
		switch ext {
		case ".mp4", ".webm", ".mkv", ".avi":
			if file.Length() > bestSize {
				bestSize = file.Length()
				bestIndex = i
			}
		}
	}
	return bestIndex
}

// workerEncryptKey is the AES-GCM key derived from the STREAM_SESSION_SECRET
// environment variable. It MUST match the secret configured on the CF worker
// (worker.js env.STREAM_SESSION_SECRET). Derived once at startup via SHA-256
// (same derivation as the worker's passToKey).
var workerEncryptKey = func() cipher.AEAD {
	secret := os.Getenv("STREAM_SESSION_SECRET")
	if secret == "" {
		log.Fatal("STREAM_SESSION_SECRET not set — worker encryption key cannot be derived. Set it in the environment to match worker.js.")
	}
	sum := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		log.Fatalf("workerEncryptKey: aes.NewCipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		log.Fatalf("workerEncryptKey: cipher.NewGCM: %v", err)
	}
	return gcm
}()

// encryptWithWorker produces the same proxied URL the CF worker's ?encrypt=
// endpoint would return, but does so LOCALLY with no network call. The worker
// encrypt path is pure AES-GCM with a SHA-256(passphrase) key and a random
// 12-byte nonce; we replicate that here so the worker's ?u=<token> decrypt
// path accepts our token. This eliminates the TLS handshake timeouts / EOF
// errors that happened on every remote encrypt call.
func encryptWithWorker(streamURL string) (string, error) {
	nonce := make([]byte, 12) // AES-GCM standard nonce size, matches worker
	if _, err := crand.Read(nonce); err != nil {
		return "", fmt.Errorf("encrypt: nonce: %v", err)
	}
	ciphertext := workerEncryptKey.Seal(nil, nonce, []byte(streamURL), nil)

	combined := make([]byte, 0, len(nonce)+len(ciphertext))
	combined = append(combined, nonce...)
	combined = append(combined, ciphertext...)

	token := base64.RawURLEncoding.EncodeToString(combined)
	proxied := "https://muviworker.dolniya778.workers.dev/?u=" + token
	log.Printf("Encrypted stream URL locally -> %s", proxied)
	return proxied, nil
}

// recoverHandler wraps an http.HandlerFunc so a panic in the handler (or in
// goroutines it cannot recover from) does not crash the whole process.
func recoverHandler(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rv := recover(); rv != nil {
				log.Printf("panic in handler: %v", rv)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		h(w, r)
	}
}

// Handler to add a torrent using a magnet link
func addTorrentHandler(w http.ResponseWriter, r *http.Request) {
	var request struct{ Magnet string }
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request"})
		return
	}

	if request.Magnet == "" {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "No magnet link provided"})
		return
	}

	magnet, err := resolveMagnet(request.Magnet)
	if err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	sessionID, err := addTorrentByMagnet(magnet)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "timeout") {
			status = http.StatusGatewayTimeout
		} else if strings.Contains(err.Error(), "Failed to create client") {
			status = http.StatusInternalServerError
		}
		respondWithJSON(w, status, map[string]string{"error": err.Error()})
		return
	}

	respondWithJSON(w, http.StatusOK, map[string]string{"sessionId": sessionID})
}

// processHandler automates the full flow: add magnet, pick the video file,
// build the stream URL, proxy it through the CF worker, and redirect to the
// external player. Usage: /process?magnet=<magnet link>
func processHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")

	magnetInput := r.URL.Query().Get("magnet")
	if magnetInput == "" {
		// also accept ?process= for convenience
		magnetInput = r.URL.Query().Get("process")
	}
	if magnetInput == "" {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "No magnet link provided. Usage: /process?magnet=<magnet>"})
		return
	}

	magnet, err := resolveMagnet(magnetInput)
	if err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	sessionID, err := addTorrentByMagnet(magnet)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "timeout") {
			status = http.StatusGatewayTimeout
		} else if strings.Contains(err.Error(), "Failed to create client") {
			status = http.StatusInternalServerError
		}
		respondWithJSON(w, status, map[string]string{"error": err.Error()})
		return
	}

	// Load the session to pick the video file
	sessionValue, ok := sessions.Load(sessionID)
	if !ok {
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Session not found after add"})
		return
	}
	session := sessionValue.(*TorrentSession)
	session.mu.Lock()
	session.LastUsed = time.Now()
	session.mu.Unlock()

	fileIndex := pickVideoFileIndex(session.Torrent)
	if fileIndex < 0 {
		respondWithJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "No playable video file found in torrent"})
		return
	}

	// Build the public stream URL using the request host
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// Respect X-Forwarded-Proto when behind a proxy
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := r.Host
	if host == "" {
		host = "localhost:7860"
	}
	streamURL := fmt.Sprintf("%s://%s/api/v1/torrent/%s/stream/%d", scheme, host, sessionID, fileIndex)
	log.Printf("Built stream URL: %s", streamURL)

	// Proxy through the CF worker
	proxiedURL, err := encryptWithWorker(streamURL)
	if err != nil {
		log.Printf("Worker encrypt failed: %v", err)
		respondWithJSON(w, http.StatusBadGateway, map[string]string{
			"error":     "Failed to proxy stream URL through worker. The Cloudflare worker may be down or unreachable. Please try again later.",
			"streamUrl": streamURL,
			"details":   err.Error(),
		})
		return
	}

	playerURL := fmt.Sprintf("https://playerr-one.vercel.app/?video=%s", url.QueryEscape(proxiedURL))
	log.Printf("Redirecting to player: %s", playerURL)

	// If the client wants JSON (e.g. Accept header or ?format=json), return it
	// instead of redirecting, so the UI / API consumers can use either.
	if r.URL.Query().Get("format") == "json" || strings.Contains(r.Header.Get("Accept"), "application/json") {
		respondWithJSON(w, http.StatusOK, map[string]string{
			"sessionId":  sessionID,
			"fileIndex":  strconv.Itoa(fileIndex),
			"proxiedUrl": proxiedURL,
			"playerUrl":  playerURL,
		})
		return
	}

	http.Redirect(w, r, playerURL, http.StatusFound)
}

// TorrentioStream mirrors the JSON shape returned by torrentio.strem.fun
type TorrentioStream struct {
	Title     string `json:"title"`
	InfoHash  string `json:"infoHash"`
	FileIdx   int    `json:"fileIdx,omitempty"`
	Source    string `json:"source,omitempty"`
	Seeders   int    `json:"seeders,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
}

type TorrentioResponse struct {
	Streams []TorrentioStream `json:"streams"`
}

// metaHandler proxies metadata from the Cinemeta stremio API.
// GET /api/v1/meta?type=movie|series&imdb=tt1234567
func metaHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")

	contentType := r.URL.Query().Get("type")
	imdbID := r.URL.Query().Get("imdb")
	if contentType == "" {
		contentType = "movie"
	}
	if imdbID == "" {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Missing imdb query param"})
		return
	}

	target := fmt.Sprintf("https://v3-cinemeta.strem.io/meta/%s/%s.json", contentType, imdbID)
	resp, err := http.Get(target)
	if err != nil {
		respondWithJSON(w, http.StatusBadGateway, map[string]string{"error": "Failed to reach cinemeta: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// keepaliveHandler lets clients refresh a session's LastUsed timestamp
// without streaming data. POST /api/v1/keepalive?session=<id>
func keepaliveHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")

	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Missing session param"})
		return
	}

	sessionValue, ok := sessions.Load(sessionID)
	if !ok {
		respondWithJSON(w, http.StatusNotFound, map[string]string{"error": "Session not found", "id": sessionID})
		return
	}

	session := sessionValue.(*TorrentSession)
	session.mu.Lock()
	session.LastUsed = time.Now()
	session.mu.Unlock()

	respondWithJSON(w, http.StatusOK, map[string]string{"status": "ok", "session": sessionID})
}

// magnetsHandler fetches magnet links from the Torrentio stremio addon.
// GET /api/v1/magnets?type=movie&imdb=tt1234567
// GET /api/v1/magnets?type=series&imdb=tt1234567&season=1&episode=1
func magnetsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")

	contentType := r.URL.Query().Get("type")
	imdbID := r.URL.Query().Get("imdb")
	if contentType == "" {
		contentType = "movie"
	}
	if imdbID == "" {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Missing imdb query param"})
		return
	}

	var target string
	if contentType == "series" {
		season := r.URL.Query().Get("season")
		episode := r.URL.Query().Get("episode")
		if season == "" || episode == "" {
			respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Series require season and episode params"})
			return
		}
		target = fmt.Sprintf("https://torrentio.strem.fun/sort=seeders|qualityfilter=4k/stream/series/%s:%s:%s.json", imdbID, season, episode)
	} else {
		target = fmt.Sprintf("https://torrentio.strem.fun/sort=seeders|qualityfilter=4k/stream/movie/%s.json", imdbID)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to build request: " + err.Error()})
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		respondWithJSON(w, http.StatusBadGateway, map[string]string{"error": "Failed to reach torrentio: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		respondWithJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("torrentio returned %d: %s", resp.StatusCode, string(body))})
		return
	}

	var torrResp TorrentioResponse
	if err := json.NewDecoder(resp.Body).Decode(&torrResp); err != nil {
		respondWithJSON(w, http.StatusBadGateway, map[string]string{"error": "Failed to decode torrentio response: " + err.Error()})
		return
	}

	// Build full magnet links and return a clean list
	type MagnetResult struct {
		Title     string `json:"title"`
		InfoHash  string `json:"infoHash"`
		Magnet    string `json:"magnet"`
		FileIdx   int    `json:"fileIdx"`
		Seeders   int    `json:"seeders"`
		SizeBytes int64  `json:"sizeBytes"`
		Source    string `json:"source"`
	}

	results := make([]MagnetResult, 0, len(torrResp.Streams))
	for _, s := range torrResp.Streams {
		magnet := fmt.Sprintf("magnet:?xt=urn:btih:%s&dn=%s", s.InfoHash, url.QueryEscape(s.Title))
		results = append(results, MagnetResult{
			Title:     s.Title,
			InfoHash:  s.InfoHash,
			Magnet:    magnet,
			FileIdx:   s.FileIdx,
			Seeders:   s.Seeders,
			SizeBytes: s.SizeBytes,
			Source:    s.Source,
		})
	}

	respondWithJSON(w, http.StatusOK, map[string]interface{}{
		"imdb":    imdbID,
		"type":    contentType,
		"magnets": results,
	})
}

// contentPageHandler serves the movie/series HTML page that shows magnets.
// /movie/tt1234567 or /series/tt1234567
func contentPageHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeFile(w, r, filepath.Join("client", "movie.html"))
}

// Torrent handler to serve torrent files and stream content
func torrentHandler(w http.ResponseWriter, r *http.Request) {
	// Log the entire URL path for debugging
	log.Printf("Torrent handler called with path: %s", r.URL.Path)

	// Extract sessionId and possibly fileIndex from the URL
	parts := strings.Split(r.URL.Path, "/")

	// Debug the path parts
	log.Printf("Path parts: %v (length: %d)", parts, len(parts))

	// The URL structure is /api/v1/torrent/[sessionId]/...
	if len(parts) < 5 { // Changed from 4 to 5
		log.Printf("Invalid path: not enough parts")
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid path"})
		return
	}

	// The session ID is at position 4, not 3 (because array is 0-indexed and path starts with /)
	sessionID := parts[4] // Changed from parts[3] to parts[4]

	log.Printf("Looking for session with ID: %s", sessionID)

	// Debug: Print all sessions that we have
	var sessionKeys []string
	sessions.Range(func(key, value interface{}) bool {
		keyStr, ok := key.(string)
		if ok {
			sessionKeys = append(sessionKeys, keyStr)
		}
		return true
	})
	log.Printf("Available sessions: %v", sessionKeys)

	// Get the torrent session from our sessions map
	sessionValue, ok := sessions.Load(sessionID)
	var session *TorrentSession
	if !ok {
		// Session was cleaned up or lost to restart. Try to recreate it
		// from the in-memory magnet store so the player can resume.
		log.Printf("Session not found with ID: %s, attempting recreation...", sessionID)
		recreated, err := recreateSession(sessionID)
		if err != nil {
			log.Printf("Session recreation failed for %s: %v", sessionID, err)
			respondWithJSON(w, http.StatusNotFound, map[string]string{
				"error":              "Session not found",
				"id":                 sessionID,
				"available_sessions": strings.Join(sessionKeys, ", "),
			})
			return
		}
		session = recreated
		log.Printf("Session %s recreated on demand", sessionID)
	} else {
		session = sessionValue.(*TorrentSession)
	}

	log.Printf("Found session with ID: %s", sessionID)
	session.mu.Lock()
	session.LastUsed = time.Now() // Update last used time
	session.mu.Unlock()

	// If there's a streaming request, handle it
	if len(parts) > 5 && parts[5] == "stream" { // Changed from parts[4] to parts[5]
		if len(parts) < 7 { // Changed from 6 to 7
			http.Error(w, "Invalid stream path", http.StatusBadRequest)
			return
		}

		fileIndex, err := strconv.Atoi(parts[6])

		if err != nil {
			http.Error(w, "Invalid file index", http.StatusBadRequest)
			return
		}

		if fileIndex < 0 || fileIndex >= len(session.Torrent.Files()) {
			http.Error(w, "File index out of range", http.StatusBadRequest)
			return
		}

		file := session.Torrent.Files()[fileIndex]

		// Set appropriate Content-Type based on file extension
		fileName := file.DisplayPath()
		extension := strings.ToLower(filepath.Ext(fileName))

		log.Printf("Streaming file: %s (type: %s)", fileName, extension)

		switch extension {
		case ".mp4":
			w.Header().Set("Content-Type", "video/mp4")
		case ".webm":
			w.Header().Set("Content-Type", "video/webm")
		case ".mkv":
			w.Header().Set("Content-Type", "video/x-matroska")
		case ".avi":
			w.Header().Set("Content-Type", "video/x-msvideo")
		default:
			w.Header().Set("Content-Type", "application/octet-stream")
		}

		// Add CORS headers for all content
		// Stream the file
		reader := file.NewReader()
		reader.SetResponsive()
		reader.SetReadahead(16 * 1024 * 1024)
		// Wrap reader so every read bumps LastUsed, keeping the session
		// alive during continuous playback (cleanup won't kill it).
		keepAliveReader := &keepAliveReader{
			Reader:  reader,
			session: session,
		}
		// ServeContent will close the reader when done but we need to
		// ensure it gets closed if there's a panic or other error
		defer func() {
			if closer, ok := reader.(io.Closer); ok {
				closer.Close()
			}
		}()
		http.ServeContent(w, r, fileName, time.Time{}, keepAliveReader)
		return
	}

	// If we get here, just return file list
	var files []map[string]interface{}
	for i, file := range session.Torrent.Files() {
		files = append(files, map[string]interface{}{
			"index": i,
			"name":  file.DisplayPath(),
			"size":  file.Length(),
		})
	}

	respondWithJSON(w, http.StatusOK, files)
}

// Helper function to respond with JSON
func respondWithJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// keepAliveReader wraps a torrent file reader and bumps the session's
// LastUsed timestamp on every Read call, so continuous playback keeps
// the session alive and prevents cleanup from killing it.
type keepAliveReader struct {
	Reader  io.ReadSeeker
	session *TorrentSession
}

func (k *keepAliveReader) Read(p []byte) (int, error) {
	n, err := k.Reader.Read(p)
	if n > 0 {
		k.session.mu.Lock()
		k.session.LastUsed = time.Now()
		k.session.mu.Unlock()
	}
	return n, err
}

func (k *keepAliveReader) Seek(offset int64, whence int) (int64, error) {
	k.session.mu.Lock()
	k.session.LastUsed = time.Now()
	k.session.mu.Unlock()
	return k.Reader.Seek(offset, whence)
}

// Update cleanupSessions with safer reflection
// recreateSession rebuilds a TorrentSession from the in-memory magnetStore,
// or by reconstructing a minimal magnet from the infohash (session ID) if the
// store doesn't have the full magnet (e.g. after a server restart). Each movie
// gets its own fresh session — we never reuse a dead session's torrent client.
func recreateSession(sessionID string) (*TorrentSession, error) {
	magnet, ok := magnetStore.Load(sessionID)
	if !ok {
		// Reconstruct a minimal magnet from the infohash + standard trackers.
		// The session ID IS the infohash, so we can always do this.
		magnet = fmt.Sprintf("magnet:?xt=urn:btih:%s", sessionID)
		log.Printf("Recreating session %s from infohash (no stored magnet)", sessionID)
	} else {
		log.Printf("Recreating session %s from magnet store", sessionID)
	}

	client, port, err := initTorrentWithProxy()
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %v", err)
	}

	t, err := client.AddMagnet(magnet.(string))
	if err != nil {
		releasePort(port)
		client.Close()
		return nil, fmt.Errorf("invalid magnet: %v", err)
	}

	t.AddTrackers([][]string{
		{"udp://tracker.opentrackr.org:1337/announce", "http://tracker.opentrackr.org:1337/announce"},
		{"udp://tracker.openbittorrent.com:6969/announce", "http://tracker.openbittorrent.com:80/announce"},
	})

	select {
	case <-t.GotInfo():
		log.Printf("Recreated session %s got torrent info", sessionID)
	case <-time.After(45 * time.Second):
		releasePort(port)
		client.Close()
		return nil, fmt.Errorf("timeout getting info during recreation")
	}

	fileIdx := pickVideoFileIndex(t)
	session := &TorrentSession{
		Client:   client,
		Torrent:  t,
		Port:     port,
		LastUsed: time.Now(),
		Magnet:   magnet.(string),
		FileIdx:  fileIdx,
	}
	sessions.Store(sessionID, session)
	log.Printf("Successfully recreated session: %s", sessionID)
	return session, nil
}

func cleanupSessions() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		log.Printf("Checking for unused sessions...")
		sessions.Range(func(key, value interface{}) bool {
			session := value.(*TorrentSession)

			session.mu.Lock()
			idle := time.Since(session.LastUsed)
			session.mu.Unlock()

			if idle > sessionIdleTimeout {
				releasePort(session.Port)
				session.Torrent.Drop()
				session.Client.Close()
				sessions.Delete(key)
				// Keep the magnet in magnetStore so the session can be recreated
				// on demand if a player requests it again.
				log.Printf("Removed idle session after %v: %s", sessionIdleTimeout, key)
			}
			return true
		})
		runtime.GC()
	}
}

// Test the proxy connection
func testProwlarrConnection(w http.ResponseWriter, r *http.Request) {
	// Add CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	// Handle preflight requests
	if r.Method == "OPTIONS" {
		return
	}

	var settings ProwlarrSettings
	if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	prowlarrHost := settings.ProwlarrHost
	prowlarrApiKey := settings.ProwlarrApiKey

	if prowlarrHost == "" || prowlarrApiKey == "" {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Prowlarr host or API key not set"})
		return
	}

	client := createSelectiveProxyClient()
	testURL := fmt.Sprintf("%s/api/v1/system/status", prowlarrHost)

	req, err := http.NewRequest("GET", testURL, nil)
	if err != nil {
		log.Printf("Error creating request: %v", err)
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	req.Header.Set("X-Api-Key", prowlarrApiKey)
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error making request to Prowlarr: %v", err)
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to connect to Prowlarr: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respondWithJSON(w, resp.StatusCode, map[string]string{"error": fmt.Sprintf("Prowlarr returned status %d", resp.StatusCode)})
		return
	}

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Error reading response: %v", err)
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to read Prowlarr response"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(responseBody)
}

// Search from Prowlarr
func searchFromProwlarr(w http.ResponseWriter, r *http.Request) {
	// Add CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Prowlarr-Host, X-Api-Key")

	// Handle preflight requests
	if r.Method == "OPTIONS" {
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "No search query provided"})
		return
	}

	// search movies in prowlarr
	settingsMutex.RLock()
	prowlarrHost := currentSettings.ProwlarrHost
	prowlarrApiKey := currentSettings.ProwlarrApiKey
	settingsMutex.RUnlock()

	if prowlarrHost == "" || prowlarrApiKey == "" {
		http.Error(w, "Prowlarr host or API key not set", http.StatusBadRequest)
		return
	}

	// Use the client that bypasses proxy for Prowlarr
	client := createSelectiveProxyClient()

	// Prowlarr search endpoint - looking for movie torrents
	searchURL := fmt.Sprintf("%s/api/v1/search?query=%s&limit=10", prowlarrHost, url.QueryEscape(query))

	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		log.Printf("Error creating request: %v", err)
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	req.Header.Set("X-Api-Key", prowlarrApiKey)
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error making request to Prowlarr: %v", err)
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to connect to Prowlarr: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	// Read the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Error reading response: %v", err)
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to read Prowlarr response"})
		return
	}

	if resp.StatusCode != http.StatusOK {
		respondWithJSON(w, resp.StatusCode, map[string]string{"error": fmt.Sprintf("Prowlarr returned status %d: %s", resp.StatusCode, string(body))})
		return
	}

	// Parse the JSON response and process the results
	var results []map[string]interface{}
	if err := json.Unmarshal(body, &results); err != nil {
		log.Printf("Error parsing JSON: %v", err)
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to parse Prowlarr response"})
		return
	}

	// Process the results to make them more usable by the frontend
	var processedResults []map[string]interface{}
	for _, result := range results {
		// Get title and download URL
		title, hasTitle := result["title"].(string)
		downloadUrl, hasDownloadUrl := result["downloadUrl"].(string)

		// Magnet URL might be present in some results
		magnetUrl, hasMagnet := result["magnetUrl"].(string)

		if !hasTitle || title == "" {
			// Skip results without titles
			continue
		}

		// We need at least one of download URL or magnet URL
		if (!hasDownloadUrl || downloadUrl == "") && (!hasMagnet || magnetUrl == "") {
			continue
		}

		// Create a simplified result object with just what we need
		processedResult := map[string]interface{}{
			"title": title,
		}

		// Prefer magnet URLs if available directly
		if hasMagnet && magnetUrl != "" {
			processedResult["magnetUrl"] = magnetUrl
			processedResult["directMagnet"] = true
		} else if hasDownloadUrl && downloadUrl != "" {
			processedResult["downloadUrl"] = downloadUrl
			processedResult["directMagnet"] = false
		}

		// Include optional fields if they exist
		if size, ok := result["size"].(float64); ok {
			processedResult["size"] = formatSize(size)
		}

		if seeders, ok := result["seeders"].(float64); ok {
			processedResult["seeders"] = seeders
		}

		if leechers, ok := result["leechers"].(float64); ok {
			processedResult["leechers"] = leechers
		}

		if indexer, ok := result["indexer"].(string); ok {
			processedResult["indexer"] = indexer
		}

		if publishDate, ok := result["publishDate"].(string); ok {
			processedResult["publishDate"] = publishDate
		}

		if category, ok := result["category"].(string); ok {
			processedResult["category"] = category
		}

		processedResults = append(processedResults, processedResult)
	}

	respondWithJSON(w, http.StatusOK, processedResults)
}

// Test Jackett Connection Handler
func testJackettConnection(w http.ResponseWriter, r *http.Request) {
	// Add CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	// Handle preflight requests
	if r.Method == "OPTIONS" {
		return
	}

	var settings JackettSettings
	if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	jackettHost := settings.JackettHost
	jackettApiKey := settings.JackettApiKey

	if jackettHost == "" || jackettApiKey == "" {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Jackett host or API key not set"})
		return
	}

	client := createSelectiveProxyClient()
	testURL := fmt.Sprintf("%s/api/v2.0/indexers/all/results?apikey=%s", jackettHost, jackettApiKey)
	req, err := http.NewRequest("GET", testURL, nil)
	if err != nil {
		log.Printf("Error creating request: %v", err)
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error making request to Jackett: %v", err)
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to connect to Jackett: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respondWithJSON(w, resp.StatusCode, map[string]string{"error": fmt.Sprintf("Jackett returned status %d", resp.StatusCode)})
		return
	}
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Error reading response: %v", err)
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to read Jackett response"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(responseBody)
}

// Search from Jackett
func searchFromJackett(w http.ResponseWriter, r *http.Request) {
	// Add CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	// Handle preflight requests
	if r.Method == "OPTIONS" {
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "No search query provided"})
		return
	}

	// search movies in jackett
	settingsMutex.RLock()
	jackettHost := currentSettings.JackettHost
	jackettApiKey := currentSettings.JackettApiKey
	settingsMutex.RUnlock()

	if jackettHost == "" || jackettApiKey == "" {
		http.Error(w, "Jackett host or API key not set", http.StatusBadRequest)
		return
	}

	// Use the client that bypasses proxy for Jackett
	client := createSelectiveProxyClient()

	// Jackett search endpoint - looking for movie torrents
	searchURL := fmt.Sprintf("%s/api/v2.0/indexers/all/results?Query=%s&apikey=%s", jackettHost, url.QueryEscape(query), jackettApiKey)

	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		log.Printf("Error creating request: %v", err)
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error making request to Jackett: %v", err)
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to connect to Jackett: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	// Read the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Error reading response: %v", err)
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to read Jackett response"})
		return
	}

	if resp.StatusCode != http.StatusOK {
		respondWithJSON(w, resp.StatusCode, map[string]string{"error": fmt.Sprintf("Jackett returned status %d: %s", resp.StatusCode, string(body))})
		return
	}

	var jacketResponse struct {
		Results []map[string]interface{} `json:"Results"`
	}

	// Parse the JSON response and process the results
	if err := json.Unmarshal(body, &jacketResponse); err != nil {
		log.Printf("Error parsing JSON: %v", err)
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to parse Jackett response"})
		return
	}

	// Process the results to make them more usable by the frontend
	var processedResults []map[string]interface{}
	for _, result := range jacketResponse.Results {
		// Get title and download URL
		title, hasTitle := result["Title"].(string)
		downloadUrl, hasDownloadUrl := result["Link"].(string)

		// Magnet URL might be present in some results
		magnetUrl, hasMagnet := result["MagnetUri"].(string)

		if !hasTitle || title == "" {
			// Skip results without titles
			continue
		}

		// We need at least one of download URL or magnet URL
		if (!hasDownloadUrl || downloadUrl == "") && (!hasMagnet || magnetUrl == "") {
			continue
		}

		// Create a simplified result object with just what we need
		processedResult := map[string]interface{}{
			"title": title,
		}

		// Prefer magnet URLs if available directly
		if hasMagnet && magnetUrl != "" && strings.HasPrefix(magnetUrl, "magnet:") {
			processedResult["magnetUrl"] = magnetUrl
			processedResult["directMagnet"] = true
		} else if hasDownloadUrl && downloadUrl != "" {
			processedResult["downloadUrl"] = downloadUrl
			processedResult["directMagnet"] = false
		}

		// Include optional fields if they exist
		if size, ok := result["Size"].(float64); ok {
			processedResult["size"] = formatSize(size)
		}

		if seeders, ok := result["Seeders"].(float64); ok {
			processedResult["seeders"] = seeders
		}

		if leechers, ok := result["Peers"].(float64); ok {
			processedResult["leechers"] = leechers
		}

		if indexer, ok := result["Tracker"].(string); ok {
			processedResult["indexer"] = indexer
		}

		if publishDate, ok := result["PublishDate"].(string); ok {
			processedResult["publishDate"] = publishDate
		}

		if category, ok := result["category"].(string); ok {
			processedResult["category"] = category
		}

		processedResults = append(processedResults, processedResult)
	}

	respondWithJSON(w, http.StatusOK, processedResults)
}

// Test Proxy Connection Handler
func testProxyConnection(w http.ResponseWriter, r *http.Request) {
	// Add CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	// Handle preflight requests
	if r.Method == "OPTIONS" {
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var settings ProxySettings
	if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	proxyURL := settings.ProxyURL

	if proxyURL == "" {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Proxy URL not set"})
		return
	}

	// Parse the proxy URL
	parsedProxyURL, err := url.Parse(proxyURL)
	if err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid proxy URL: " + err.Error()})
		return
	}

	// Create a transport that uses the proxy
	transport := &http.Transport{
		Proxy: http.ProxyURL(parsedProxyURL),
	}

	// Create client with custom transport and timeout
	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second, // Adjust timeout as needed
	}

	testURL := "https://httpbin.org/ip"
	req, err := http.NewRequest("GET", testURL, nil)
	if err != nil {
		log.Printf("Error creating request: %v", err)
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error making request through proxy: %v", err)
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Proxy connection failed: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Error reading response: %v", err)
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to read proxy response"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(responseBody)
}

// Helper function to save settings to file (assumes mutex is already locked)
func saveSettingsToFile() error {
	// Create the directory if it doesn't exist
	if err := os.MkdirAll("config", 0755); err != nil {
		log.Fatalf("Failed to create config directory: %v", err)
	}

	file, err := os.Create("config/settings.json")
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(currentSettings); err != nil {
		return err
	}

	return nil
}

// Proxy Settings Save Handler
func saveProxySettingsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var newSettings ProxySettings
	if err := json.NewDecoder(r.Body).Decode(&newSettings); err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	settingsMutex.RLock()
	currentSettings.EnableProxy = newSettings.EnableProxy
	currentSettings.ProxyURL = newSettings.ProxyURL
	defer settingsMutex.RUnlock()

	if err := saveSettingsToFile(); err != nil {
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to save settings: " + err.Error()})
		return
	}
	println("Proxy settings saved successfully")

	setGlobalProxy()

	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Proxy settings saved successfully"})
}

// Prowlarr Settings Save Handler
func saveProwlarrSettingsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var newSettings ProwlarrSettings
	if err := json.NewDecoder(r.Body).Decode(&newSettings); err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	settingsMutex.RLock()
	currentSettings.EnableProwlarr = newSettings.EnableProwlarr
	currentSettings.ProwlarrHost = newSettings.ProwlarrHost
	currentSettings.ProwlarrApiKey = newSettings.ProwlarrApiKey
	defer settingsMutex.RUnlock()

	if err := saveSettingsToFile(); err != nil {
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to save settings: " + err.Error()})
		return
	}

	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Prowlarr settings saved successfully"})
}

// Jackett Settings Save Handler
func saveJackettSettingsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var newSettings JackettSettings
	if err := json.NewDecoder(r.Body).Decode(&newSettings); err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	settingsMutex.RLock()
	currentSettings.EnableJackett = newSettings.EnableJackett
	currentSettings.JackettHost = newSettings.JackettHost
	currentSettings.JackettApiKey = newSettings.JackettApiKey
	defer settingsMutex.RUnlock()

	if err := saveSettingsToFile(); err != nil {
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to save settings: " + err.Error()})
		return
	}

	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Jackett settings saved successfully"})
}

// Convert Torrent to Magnet Handler
func convertTorrentToMagnetHandler(w http.ResponseWriter, r *http.Request) {
	// Set CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == "OPTIONS" {
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse multipart form with 10MB memory limit
	const maxUploadSize = 10 << 20 // 10MB
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Failed to parse form: " + err.Error()})
		return
	}

	// Get the torrent file from the form data
	file, header, err := r.FormFile("torrent")
	if err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Missing torrent file"})
		return
	}
	defer file.Close()

	// Check file size
	if header.Size > maxUploadSize {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "File too large"})
		return
	}

	// Read the torrent file content
	fileBytes, err := io.ReadAll(file)
	if err != nil {
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to read file"})
		return
	}

	// Parse torrent file
	mi, err := metainfo.Load(bytes.NewReader(fileBytes))
	if err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid torrent file: " + err.Error()})
		return
	}

	// Get info hash
	infoHash := mi.HashInfoBytes().String()

	// Build magnet URL components
	magnet := fmt.Sprintf("magnet:?xt=urn:btih:%s", infoHash)

	// Add display name
	info, err := mi.UnmarshalInfo()
	if err == nil {
		magnet += fmt.Sprintf("&dn=%s", url.QueryEscape(info.Name))
	}

	// Add trackers
	for _, tier := range mi.AnnounceList {
		for _, tracker := range tier {
			magnet += fmt.Sprintf("&tr=%s", url.QueryEscape(tracker))
		}
	}

	respondWithJSON(w, http.StatusOK, map[string]string{
		"magnet": magnet,
	})
}