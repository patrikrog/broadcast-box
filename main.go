package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/patrikrog/broadcast-box/internal/networktest"
	"github.com/patrikrog/broadcast-box/internal/webrtc"
)

const (
	envFileProd = ".env.production"
	envFileDev  = ".env.development"

	networkTestIntroMessage   = "\033[0;33mNETWORK_TEST_ON_START is enabled. If the test fails Broadcast Box will exit.\nSee the README for how to debug or disable NETWORK_TEST_ON_START\033[0m"
	networkTestSuccessMessage = "\033[0;32mNetwork Test passed.\nHave fun using Broadcast Box.\033[0m"
	networkTestFailedMessage  = "\033[0;31mNetwork Test failed.\n%s\nPlease see the README and join Discord for help\033[0m"
)

var dbPool *pgxpool.Pool


type (
	whepLayerRequestJSON struct {
		MediaId    string `json:"mediaId"`
		EncodingId string `json:"encodingId"`
	}
)

func logHTTPError(w http.ResponseWriter, err string, code int) {
	log.Println(err)
	http.Error(w, err, code)
}

func validateStreamKey(streamKey string) bool {
	return regexp.MustCompile(`^[a-zA-Z0-9_\-\.~]+$`).MatchString(streamKey)
}

func extractBearerToken(authHeader string) ([]string, bool) {
	const bearerPrefix = "Bearer "
	if strings.HasPrefix(authHeader, bearerPrefix) {
		s := strings.Split(strings.TrimPrefix(authHeader, bearerPrefix), ";")
		return s, true
	}
	return nil, false
}

func whipHandler(res http.ResponseWriter, r *http.Request) {
	if r.Method == "DELETE" {
		return
	}

	streamKeyHeader := r.Header.Get("Authorization")
	if streamKeyHeader == "" {
		logHTTPError(res, "Authorization was not set", http.StatusBadRequest)
		return
	}

	token, ok := extractBearerToken(streamKeyHeader)
	if !ok || token == nil || !validateStreamKey(token[0]) {
		logHTTPError(res, "Not a valid token", http.StatusBadRequest)
	}

	streamer := webrtc.NewStreamer(dbPool, r.Context(), token)
	if streamer == nil {
		logHTTPError(res, "Not an authorized streamer", http.StatusForbidden)
		return
	}

	offer, err := io.ReadAll(r.Body)
	if err != nil {
		logHTTPError(res, err.Error(), http.StatusBadRequest)
		return
	}

	answer, err := webrtc.WHIP(string(offer), streamer)
	if err != nil {
		logHTTPError(res, err.Error(), http.StatusBadRequest)
		return
	}

	res.Header().Add("Location", "/api/whip")
	res.Header().Add("Content-Type", "application/sdp")
	res.WriteHeader(http.StatusCreated)
	fmt.Fprint(res, answer)
}

func whepHandler(res http.ResponseWriter, req *http.Request) {
	streamKeyHeader := req.Header.Get("Authorization")
	if streamKeyHeader == "" {
		logHTTPError(res, "Authorization was not set", http.StatusBadRequest)
		return
	}

	token, ok := extractBearerToken(streamKeyHeader)
	if !ok || !validateStreamKey(token[0]) {
		logHTTPError(res, "Invalid stream key format", http.StatusBadRequest)
		return
	}

	offer, err := io.ReadAll(req.Body)
	if err != nil {
		logHTTPError(res, err.Error(), http.StatusBadRequest)
		return
	}

	answer, whepSessionId, err := webrtc.WHEP(string(offer), token[0])
	if err != nil {
		logHTTPError(res, err.Error(), http.StatusBadRequest)
		return
	}

	apiPath := req.Host + strings.TrimSuffix(req.URL.RequestURI(), "whep")
	res.Header().Add("Link", `<`+apiPath+"sse/"+whepSessionId+`>; rel="urn:ietf:params:whep:ext:core:server-sent-events"; events="layers"`)
	res.Header().Add("Link", `<`+apiPath+"layer/"+whepSessionId+`>; rel="urn:ietf:params:whep:ext:core:layer"`)
	res.Header().Add("Location", "/api/whep")
	res.Header().Add("Content-Type", "application/sdp")
	res.WriteHeader(http.StatusCreated)
	fmt.Fprint(res, answer)
}

func whepServerSentEventsHandler(res http.ResponseWriter, req *http.Request) {
	res.Header().Set("Content-Type", "text/event-stream")
	res.Header().Set("Cache-Control", "no-cache")
	res.Header().Set("Connection", "keep-alive")

	vals := strings.Split(req.URL.RequestURI(), "/")
	whepSessionId := vals[len(vals)-1]

	layers, err := webrtc.WHEPLayers(whepSessionId)
	if err != nil {
		logHTTPError(res, err.Error(), http.StatusBadRequest)
		return
	}

	fmt.Fprint(res, "event: layers\n")
	fmt.Fprintf(res, "data: %s\n", string(layers))
	fmt.Fprint(res, "\n\n")
}

func whepLayerHandler(res http.ResponseWriter, req *http.Request) {
	var r whepLayerRequestJSON
	if err := json.NewDecoder(req.Body).Decode(&r); err != nil {
		logHTTPError(res, err.Error(), http.StatusBadRequest)
		return
	}

	vals := strings.Split(req.URL.RequestURI(), "/")
	whepSessionId := vals[len(vals)-1]

	if err := webrtc.WHEPChangeLayer(whepSessionId, r.EncodingId); err != nil {
		logHTTPError(res, err.Error(), http.StatusBadRequest)
		return
	}
}

func streamsHandler(res http.ResponseWriter, req *http.Request) {
	res.Header().Add("Content-Type", "application/json")

	streamKeys, err := webrtc.GetStreamKeys(dbPool, req.Context())
	if err != nil {
		logHTTPError(res, "Could not get stream keys", http.StatusBadRequest)
		return
	}

	if err := json.NewEncoder(res).Encode(streamKeys); err != nil {
		logHTTPError(res, err.Error(), http.StatusBadRequest)
		return
	}
}

func statusHandler(res http.ResponseWriter, req *http.Request) {
	res.Header().Add("Content-Type", "application/json")
	streamKey := req.PathValue("streamkey")

	if !validateStreamKey(streamKey) {
		logHTTPError(res, "Invalid stream key format", http.StatusBadRequest)
		return
	}

	streamKeys, err := webrtc.GetStreamKeys(dbPool, req.Context())
	if err != nil {
		logHTTPError(res, "Could not get stream keys", http.StatusBadRequest)
		return
	}


	if !slices.Contains(streamKeys, streamKey) {
		logHTTPError(res, "Stream does not exist", http.StatusNotFound)
		return
	}
	if err := json.NewEncoder(res).Encode(webrtc.GetStreamStatus(streamKey)); err != nil {
		logHTTPError(res, err.Error(), http.StatusBadRequest)
		return
	}
}

func corsHandler(next func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(res http.ResponseWriter, req *http.Request) {
		res.Header().Set("Access-Control-Allow-Origin", "*")
		res.Header().Set("Access-Control-Allow-Methods", "*")
		res.Header().Set("Access-Control-Allow-Headers", "*")
		res.Header().Set("Access-Control-Expose-Headers", "*")

		if req.Method != http.MethodOptions {
			next(res, req)
		}
	}
}

func main() {
	loadConfigs := func() error {
		if os.Getenv("APP_ENV") == "development" {
			log.Println("Loading `" + envFileDev + "`")
			return godotenv.Load(envFileDev)
		} else {
			log.Println("Loading `" + envFileProd + "`")
			if err := godotenv.Load(envFileProd); err != nil {
				return err
			}

			return nil
		}
	}

	if err := loadConfigs(); err != nil {
		log.Println("Failed to find config in CWD, changing CWD to executable path")

		exePath, err := os.Executable()
		if err != nil {
			log.Fatal(err)
		}

		if err = os.Chdir(filepath.Dir(exePath)); err != nil {
			log.Fatal(err)
		}

		if err = loadConfigs(); err != nil {
			log.Fatal(err)
		}
	}
	var err error
	dbPool, err = pgxpool.New(context.Background(), os.Getenv("POSTGRES_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer dbPool.Close()

	webrtc.Configure()

	if os.Getenv("NETWORK_TEST_ON_START") == "true" {
		fmt.Println(networkTestIntroMessage) //nolint

		go func() {
			time.Sleep(time.Second * 5)

			if networkTestErr := networktest.Run(whepHandler); networkTestErr != nil {
				fmt.Printf(networkTestFailedMessage, networkTestErr.Error())
				os.Exit(1)
			} else {
				fmt.Println(networkTestSuccessMessage) //nolint
			}
		}()
	}

	httpsRedirectPort := "80"
	if val := os.Getenv("HTTPS_REDIRECT_PORT"); val != "" {
		httpsRedirectPort = val
	}

	if os.Getenv("HTTPS_REDIRECT_PORT") != "" || os.Getenv("ENABLE_HTTP_REDIRECT") != "" {
		go func() {
			redirectServer := &http.Server{
				Addr: ":" + httpsRedirectPort,
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, "https://"+r.Host+r.URL.String(), http.StatusMovedPermanently)
				}),
			}

			log.Println("Running HTTP->HTTPS redirect Server at :" + httpsRedirectPort)
			log.Fatal(redirectServer.ListenAndServe())
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/streams", corsHandler(streamsHandler))
	mux.HandleFunc("/api/status/{streamkey}", corsHandler(statusHandler))
	mux.HandleFunc("/api/whip", corsHandler(whipHandler))
	mux.HandleFunc("/api/whep", corsHandler(whepHandler))
	mux.HandleFunc("/api/sse/", corsHandler(whepServerSentEventsHandler))
	mux.HandleFunc("/api/layer/", corsHandler(whepLayerHandler))

	server := &http.Server{
		Handler: mux,
		Addr:    os.Getenv("HTTP_ADDRESS"),
	}

	tlsKey := os.Getenv("SSL_KEY")
	tlsCert := os.Getenv("SSL_CERT")

	if tlsKey != "" && tlsCert != "" {
		server.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{},
		}

		cert, err := tls.LoadX509KeyPair(tlsCert, tlsKey)
		if err != nil {
			log.Fatal(err)
		}

		server.TLSConfig.Certificates = append(server.TLSConfig.Certificates, cert)

		log.Println("Running HTTPS Server at `" + os.Getenv("HTTP_ADDRESS") + "`")
		log.Fatal(server.ListenAndServeTLS("", ""))
	} else {
		log.Println("Running HTTP Server at `" + os.Getenv("HTTP_ADDRESS") + "`")
		log.Fatal(server.ListenAndServe())
	}
}
