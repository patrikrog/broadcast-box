package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
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

var errNoBuildDirectoryErr = errors.New("\033[0;31mBuild directory does not exist, run `npm install` and `npm run build` in the web directory.\033[0m")

var (
	dbPool *pgxpool.Pool
	dbCtx context.Context
	dbErr error
)


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

func validateStreamer(token []string) (*webrtc.Streamer, bool) {
	conn, err := dbPool.Acquire(dbCtx)
	if err != nil {
		log.Fatal(err)
	}

	streamer, ok := webrtc.LookupStreamer(conn, dbCtx, token)
	return streamer, ok
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
	streamer, ok := validateStreamer(token)
	if !ok || streamer == nil {
		logHTTPError(res, "Not an authorized streamer", http.StatusForbidden)
		return
	}

	offer, err := io.ReadAll(r.Body)
	if err != nil {
		logHTTPError(res, err.Error(), http.StatusBadRequest)
		return
	}

	answer, err := webrtc.WHIP(string(offer), streamer.StreamKey)
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

func statusHandler(res http.ResponseWriter, req *http.Request) {
	res.Header().Add("Content-Type", "application/json")

	if err := json.NewEncoder(res).Encode(webrtc.GetStreamStatuses()); err != nil {
		logHTTPError(res, err.Error(), http.StatusBadRequest)
	}
}

func indexHTMLWhenNotFound(fs http.FileSystem) http.Handler {
	fileServer := http.FileServer(fs)

	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		_, err := fs.Open(path.Clean(req.URL.Path)) // Do not allow path traversals.
		if errors.Is(err, os.ErrNotExist) {
			http.ServeFile(resp, req, "./web/build/index.html")

			return
		}
		fileServer.ServeHTTP(resp, req)
	})
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

			if _, err := os.Stat("./web/build"); os.IsNotExist(err) && os.Getenv("DISABLE_FRONTEND") == "" {
				return errNoBuildDirectoryErr
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
	dbCtx = context.Background()
	dbPool, dbErr = pgxpool.New(dbCtx, os.Getenv("POSTGRES_URL"))
	if dbErr != nil {
		log.Fatal(dbErr)
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
	if os.Getenv("DISABLE_FRONTEND") == "" {
		mux.Handle("/", indexHTMLWhenNotFound(http.Dir("./web/build")))
	}
	mux.HandleFunc("/api/whip", corsHandler(whipHandler))
	mux.HandleFunc("/api/whep", corsHandler(whepHandler))
	mux.HandleFunc("/api/sse/", corsHandler(whepServerSentEventsHandler))
	mux.HandleFunc("/api/layer/", corsHandler(whepLayerHandler))

	if os.Getenv("DISABLE_STATUS") == "" {
		mux.HandleFunc("/api/status", corsHandler(statusHandler))
	}

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
