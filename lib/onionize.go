// onionize.go - onionize directories, files and zips.
//
// To the extent possible under law, Ivan Markin waived all copyright
// and related or neighboring rights to this module of onionize, using the creative
// commons "cc0" public domain dedication. See LICENSE or
// <http://creativecommons.org/publicdomain/zero/1.0/> for full details.

package onionize

import (
	"archive/zip"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/nogoegst/bulb"
	"github.com/nogoegst/onionutil"
	"github.com/nogoegst/pickfs"
	"golang.org/x/tools/godoc/vfs"
	"golang.org/x/tools/godoc/vfs/httpfs"
	"golang.org/x/tools/godoc/vfs/zipfs"
)

const slugLengthB32 = 16

type Parameters struct {
	Path            string
	Zip             bool
	Slug            bool
	ControlPath     string
	ControlPassword string
	Passphrase      string
	Debug           bool
}

func ResetHTTPConn(w *http.ResponseWriter) error {
	hj, ok := (*w).(http.Hijacker)
	if !ok {
		return fmt.Errorf("This webserver doesn't support hijacking")
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

func CheckAndRewriteSlug(req *http.Request, slug string) error {
	if slug == "" {
		return nil
	}
	reqURL := strings.TrimLeft(req.URL.String(), "/")
	if len(reqURL) < len(slug) {
		return fmt.Errorf("URL is too short to have a slug in it")
	}
	if 1 != subtle.ConstantTimeCompare([]byte(slug), []byte(reqURL[:len(slug)])) {
		return fmt.Errorf("Wrong slug")
	}
	reqURL = strings.TrimPrefix(reqURL, slug)
	req.URL, _ = neturl.Parse(reqURL)
	return nil
}

type ResultLink struct {
	URL   string
	Error error
}

func Onionize(p Parameters, linkCh chan<- ResultLink) {
	var fs vfs.FileSystem
	var url string
	var slug string
	if p.Slug {
		slugBin := make([]byte, (slugLengthB32*5)/8+1)
		_, err := rand.Read(slugBin)
		if err != nil {
			linkCh <- ResultLink{Error: fmt.Errorf("Unable to generate slug: %v", err)}
			return
		}
		slug = onionutil.Base32Encode(slugBin)[:slugLengthB32]
		url += slug + "/"
	}

	if p.Zip {
		// Serve contents of zip archive
		rcZip, err := zip.OpenReader(p.Path)
		if err != nil {
			linkCh <- ResultLink{Error: fmt.Errorf("Unable to open zip archive: %v", err)}
			return
		}
		fs = zipfs.New(rcZip, "onionize")
	} else {
		fileInfo, err := os.Stat(p.Path)
		if err != nil {
			linkCh <- ResultLink{Error: fmt.Errorf("Unable to open path: %v", err)}
			return
		}
		if fileInfo.IsDir() {
			// Serve a plain directory
			fs = vfs.OS(p.Path)
		} else {
			// Serve just one file in OnionShare-like manner
			abspath, err := filepath.Abs(p.Path)
			if err != nil {
				linkCh <- ResultLink{Error: fmt.Errorf("Unable to get absolute path to file")}
				return
			}
			dir, file := filepath.Split(abspath)
			m := make(map[string]string)
			m[file] = file
			fs = pickfs.New(vfs.OS(dir), m)
			// Escape URL to be safe and copypasteble
			escapedFilename := strings.Replace(neturl.QueryEscape(file), "+", "%20", -1)
			url += escapedFilename
		}
	}
	// Serve our virtual filesystem
	fileserver := http.FileServer(httpfs.New(fs))
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if p.Debug {
			log.Printf("Request for \"%s\"", req.URL)
		}
		err := CheckAndRewriteSlug(req, slug)
		if err != nil {
			if p.Debug {
				log.Print(err)
			}
			err := ResetHTTPConn(&w)
			if err != nil {
				log.Printf("Unable to reset connection: %v", err)
			}
			return
		}
		if req.URL.String() == "" { // empty root path
			http.Redirect(w, req, "/"+slug+"/", http.StatusFound)
		}
		if p.Debug {
			log.Printf("Rewriting URL to \"%s\"", req.URL)
		}
		fileserver.ServeHTTP(w, req)
	})
	server := &http.Server{Handler: mux}

	// Connect to a running tor instance
	c, err := bulb.DialURL(p.ControlPath)
	if err != nil {
		linkCh <- ResultLink{Error: fmt.Errorf("Failed to connect to control socket: %v", err)}
		return
	}
	defer c.Close()

	// See what's really going on under the hood
	c.Debug(p.Debug)

	// Authenticate with the control port
	if err := c.Authenticate(p.ControlPassword); err != nil {
		linkCh <- ResultLink{Error: fmt.Errorf("Authentication failed: %v", err)}
		return
	}
	// Derive onion service keymaterial from passphrase or generate a new one
	aocfg := &bulb.NewOnionConfig{
		DiscardPK:      true,
		AwaitForUpload: true,
	}
	if p.Passphrase != "" {
		keyrd, err := onionutil.KeystreamReader([]byte(p.Passphrase), []byte("onionize-keygen"))
		if err != nil {
			linkCh <- ResultLink{Error: fmt.Errorf("Unable to create keystream: %v", err)}
			return
		}
		privOnionKey, err := onionutil.GenerateOnionKey(keyrd)
		if err != nil {
			linkCh <- ResultLink{Error: fmt.Errorf("Unable to generate onion key: %v", err)}
			return
		}
		aocfg.PrivateKey = privOnionKey
	}
	onionListener, err := c.NewListener(aocfg, 80)
	if err != nil {
		linkCh <- ResultLink{Error: fmt.Errorf("Error occured while creating an onion service: %v", err)}
		return
	}
	defer onionListener.Close()
	// Track if tor went down
	go func() {
		for {
			_, err := c.NextEvent()
			if err != nil {
				log.Fatalf("Lost connection to tor: %v", err)
			}
		}
	}()
	onionHost := strings.TrimSuffix(onionListener.Addr().String(), ":80")

	// Return the link to the service
	linkCh <- ResultLink{URL: fmt.Sprintf("http://%s/%s", onionHost, url)}
	// Run a webservice
	err = server.Serve(onionListener)
	if err != nil {
		log.Fatalf("Cannot serve HTTP")
	}
}