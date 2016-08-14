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
	"sort"
	"strings"
	"sync"
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
	flagRate     = flag.Duration("rate", time.Second, "To limit the number of processes created to no more than one per given duration. Set to 0 for no limit.")
)

var (
	rootdir, _ = os.Getwd()
	up         *basicauth.UserPass

	childrenMu sync.RWMutex
	children   map[time.Time]*os.Process

	// TODO(mpl): rate limit per source ip instead of for all requests?
	lastRunMu sync.RWMutex
	lastRun   time.Time
)

func usage() {
	fmt.Fprintf(os.Stderr, "\t httprunner \n")
	flag.PrintDefaults()
	fmt.Fprint(os.Stderr, "The endpoints are /run, /kill, and /die.\n")
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
	deadline time.Time
	limit    int
	sum      int

	bufMu sync.Mutex
	buf   *bytes.Buffer

	discardingMu sync.RWMutex
	discarding   bool
}

func (lw limitWriter) Write(p []byte) (n int, err error) {
	lw.discardingMu.RLock()
	if lw.discarding {
		lw.discardingMu.RUnlock()
		return ioutil.Discard.Write(p)
	}
	lw.discardingMu.RUnlock()
	lw.bufMu.Lock()
	n, err = lw.buf.Write(p)
	lw.bufMu.Unlock()
	lw.sum += n
	if lw.sum > lw.limit {
		lw.discardingMu.Lock()
		lw.discarding = true
		lw.discardingMu.Unlock()
	}
	return
}

func (lw limitWriter) Read(p []byte) (n int, err error) {
	lw.discardingMu.RLock()
	if lw.discarding {
		lw.discardingMu.RUnlock()
		return 0, io.EOF
	}
	lw.discardingMu.RUnlock()
	lw.bufMu.Lock()
	defer lw.bufMu.Unlock()
	return lw.buf.Read(p)
}

func killChildren() {
	childrenMu.Lock()
	defer childrenMu.Unlock()
	for _, v := range children {
		if err := v.Kill(); err != nil {
			log.Printf("couldn't kill child: %v", err)
		}
	}
	children = make(map[time.Time]*os.Process)
}

func handleKillAll(w http.ResponseWriter, r *http.Request) {
	killChildren()
	if _, err := io.Copy(w, strings.NewReader("They have left for a better world.")); err != nil {
		log.Print(err)
	}
}

func handleDie(w http.ResponseWriter, r *http.Request) {
	killChildren()
	sayonara := "The sweet embrace of death, finally."
	if _, err := io.Copy(w, strings.NewReader(sayonara)); err != nil {
		log.Print(err)
	}
	log.Print(sayonara)
	time.Sleep(time.Second)
	os.Exit(0)
}

type times []time.Time

func (t times) Len() int           { return len(t) }
func (t times) Swap(i, j int)      { t[i], t[j] = t[j], t[i] }
func (t times) Less(i, j int) bool { return t[i].Before(t[j]) }

func handleList(w http.ResponseWriter, r *http.Request) {
	childrenMu.RLock()
	defer childrenMu.RUnlock()
	var t times
	for k, _ := range children {
		t = append(t, k)
	}
	sort.Sort(t)
	var out bytes.Buffer
	for _, pt := range t {
		if _, err := out.WriteString(fmt.Sprintf("%s : %d\n", pt.Format(time.RFC3339), children[pt].Pid)); err != nil {
			http.Error(w, "can't print children list", http.StatusInternalServerError)
			return
		}
	}
	if _, err := io.Copy(w, &out); err != nil {
		log.Printf("error listing children: %v", err)
	}
}

func handleCommand(w http.ResponseWriter, r *http.Request) {
	if *flagRate != 0 {
		lastRunMu.RLock()
		if time.Now().Before(lastRun.Add(*flagRate)) {
			http.Error(w, "Command process creation is rate limited", http.StatusTooManyRequests)
			lastRunMu.RUnlock()
			return
		}
		lastRunMu.RUnlock()
	}
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
	log.Printf("Started %v with pid %v", args[0], cmd.Process.Pid)
	startTime := time.Now()
	childrenMu.Lock()
	children[startTime] = cmd.Process
	childrenMu.Unlock()
	lastRunMu.Lock()
	lastRun = time.Now()
	lastRunMu.Unlock()
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("%v failed: %v, %v", args[0], err, berr.String())
		}
		childrenMu.Lock()
		delete(children, startTime)
		childrenMu.Unlock()
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
	children = make(map[time.Time]*os.Process)

	http.Handle("/run", makeHandler(handleCommand))
	http.Handle("/kill", makeHandler(handleKillAll))
	http.Handle("/die", makeHandler(handleDie))
	http.Handle("/ls", makeHandler(handleList))
	if err := http.ListenAndServeTLS(
		*flagHost,
		filepath.Join(os.Getenv("HOME"), "keys", "cert.pem"),
		filepath.Join(os.Getenv("HOME"), "keys", "key.pem"),
		nil); err != nil {
		log.Fatal(err)
	}
}
