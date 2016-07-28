package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mpl/basicauth"
)

const (
	idstring = "http://golang.org/pkg/http/#ListenAndServe"
)

var (
	flagHost     = flag.String("host", "0.0.0.0:8080", "listening port and hostname")
	flagHelp     = flag.Bool("h", false, "show this help")
	flagUserpass = flag.String("userpass", "", "optional username:password protection")
	flagCommand  = flag.String("command", "", "The command to run")
)

var (
	rootdir, _ = os.Getwd()
	up         *basicauth.UserPass
)

func usage() {
	fmt.Fprintf(os.Stderr, "\t httprunner \n")
	flag.PrintDefaults()
	os.Exit(2)
}

func makeHandler(fn func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if e, ok := recover().(error); ok {
				http.Error(w, e.Error(), http.StatusInternalServerError)
				return
			}
		}()
		w.Header().Set("Server", idstring)
		if isAllowed(r) {
			fn(w, r)
		} else {
			basicauth.SendUnauthorized(w, r, "httprunner")
		}
	}
}

func isAllowed(r *http.Request) bool {
	if *flagUserpass == "" {
		return true
	}
	return up.IsAllowed(r)
}

func initUserPass() {
	if *flagUserpass == "" {
		return
	}
	var err error
	up, err = basicauth.New(*flagUserpass)
	if err != nil {
		log.Fatal(err)
	}
}

// TODO(mpl): have a look at https://github.com/cespare/window

type limitWriter struct {
	deadline   time.Time
	limit      int
	sum        int
	buf        *bytes.Buffer
	discarding bool
}

func (lw limitWriter) Write(p []byte) (n int, err error) {
	if lw.discarding {
		return ioutil.Discard.Write(p)
	}
	n, err = lw.buf.Write(p)
	lw.sum += n
	if lw.sum > lw.limit {
		lw.discarding = true
	}
	return
}

func (lw limitWriter) Read(p []byte) (n int, err error) {
	return lw.buf.Read(p)
}

func handleCommand(w http.ResponseWriter, r *http.Request) {
	// TODO(mpl): be less lazy about the doubled spaces, and probably other things.
	args := strings.Fields(*flagCommand)
	cmd := exec.Command(args[0], args[1:]...)
	var buf, berr bytes.Buffer
	lw := limitWriter{
		limit: 1 << 20,
		buf:   &buf,
	}
	stdout := io.MultiWriter(os.Stdout, lw)
	cmd.Stdout = stdout
	cmd.Stderr = &berr
	if err := cmd.Start(); err != nil {
		log.Printf("%v failed to start: %v, %v", args[0], err, berr.String())
		return
	}
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("%v failed: %v, %v", args[0], err, berr.String())
		}
	}()
	var bufout bytes.Buffer
	sendResponse := func(b *bytes.Buffer) {
		var response io.Reader
		if b.Len() > 0 {
			response = b
		} else {
			response = strings.NewReader("Command started but no output yet.")
		}
		if _, err := io.Copy(w, response); err != nil {
			log.Printf("response copy error: %v", err)
		}
	}
	var seenData bool
	// TODO(mpl): test if we could relax both these times now that we're sending the header asap.
	maxIdle := 200 * time.Millisecond
	t := time.After(1 * time.Second)
	lastDataTime := time.Now()
	for {
		select {
		case <-t:
			sendResponse(&bufout)
			return
		default:
		}
		n, err := io.Copy(&bufout, lw)
		if err != nil {
			log.Printf("output copy error: %v", err)
			break
		}
		if n > 0 {
			if !seenData {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				seenData = true
			}
			lastDataTime = time.Now()
		} else {
			if lastDataTime.Add(maxIdle).Before(time.Now()) {
				log.Printf("no output for more than %v, wrapping up.", maxIdle)
				break
			}
		}
	}
	sendResponse(&bufout)
}

func main() {
	flag.Usage = usage
	flag.Parse()
	if *flagHelp {
		usage()
	}
	nargs := flag.NArg()
	if nargs > 0 {
		usage()
	}
	if *flagCommand == "" {
		fmt.Printf("No command to run")
		usage()
	}

	initUserPass()

	http.Handle("/run", makeHandler(handleCommand))
	if err := http.ListenAndServeTLS(
		*flagHost,
		filepath.Join(os.Getenv("HOME"), "keys", "cert.pem"),
		filepath.Join(os.Getenv("HOME"), "keys", "key.pem"),
		nil); err != nil {
		log.Fatal(err)
	}
}
