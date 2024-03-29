package main

import (
	"bytes"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

type db interface {
	add(string, *channel) error
	get(string) (*channel, error)
	rm(string) error
	subs() ([]*channel, error)
}

type localDb struct {
	sync.RWMutex
	c map[string]*channel
}

type listenersChan chan net.Listener

type video struct {
	Title       string `xml:"title"`
	VideoId     string `xml:"videoId"`
	Description string `xml:"group>description"`
}
type videos []*video
type channel struct {
	Name   string
	URL    string
	Videos []*video `xml:"entry"`
}

type endpoint struct {
	sockName string
	handle   func(net.Listener)
	blocking bool
}

type state byte

const (
	failure state = iota
	success
)

const (
	listenersSize             = 8
	maxFeedSize               = 7
	channelURLBase     string = "https://www.youtube.com/@"
	channelURLByIDBase string = "https://www.youtube.com/channel/"
	feedURLBase        string = "https://www.youtube.com/feeds/videos.xml?channel_id="
	watchURLStr        string = "https://www.youtube.com/watch?v="
	querySearchURLBase string = "https://www.youtube.com/results?search_query="
)

var logTo *os.File

var (
	listeners listenersChan = listenersChan(make(chan net.Listener, listenersSize))
	feed      *localDb      = &localDb{c: make(map[string]*channel)}

	flagNotify      *bool
	flagRefreshRate *int
)

func (lc listenersChan) add(l net.Listener) {
	if len(lc)+1 > listenersSize {
		fmt.Fprintf(os.Stderr, "[ERROR]: too many listeners opened\n")
		l.Close()
		lc.close()
		os.Exit(1)
	}
	lc <- l
}

func (lc listenersChan) close() {
	if len(lc) == 0 {
		return
	}
	for i := 0; i < listenersSize; i++ {
		l := <-lc
		fmt.Fprintf(logTo, "[INFO]: closing listener: %v\n", l.Addr().String())
		l.Close()
	}
	close(lc)
}

func (v *video) String() string {
	return fmt.Sprintf("%v\n\t%v%v\n", v.Title, watchURLStr, v.VideoId)
}

func (vs videos) String() string {
	var videosStr string
	for _, v := range vs {
		videosStr += v.String()
	}
	if len(videosStr) > 0 {
		videosStr = videosStr[:len(videosStr)-1] // NOTE: rm last newline
	}
	return videosStr
}

func (c *channel) String() string {
	return fmt.Sprintf("%v\n\t%v\n\n%v", c.Name, c.URL, videos(c.Videos).String())
}

func (ep endpoint) serve() {
	if err := os.RemoveAll(ep.sockName); err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR]: failed to remove all from socket '%v': %v\n", ep.sockName, err)
		return
	}
	listener, err := net.Listen("unix", ep.sockName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR]: failed to listen to socket '%v': %v\n", ep.sockName, err)
		return
	}
	listeners.add(listener)
	if ep.blocking {
		ep.handle(listener)
	} else {
		go ep.handle(listener)
	}
}

func subsFromFile(fname string) {
	if fname == "" {
		fmt.Fprintf(os.Stderr, "[INFO]: no subs filename provided\n")
		return
	}
	f, err := os.Open(fname)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR]: failed to open '%v': %v\n", fname, err)
		return
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR]: failed to seek end in '%v': %v\n", fname, err)
		return
	}
	f.Seek(0, io.SeekStart)
	buf := make([]byte, size)
	n, err := f.Read(buf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR]: failed to read file '%v': %v\n", fname, err)
		return
	}
	if int64(n) != size {
		fmt.Fprintf(os.Stderr, "[WARNING]: file '%v' size=%v, but read=%v\n", fname, size, n)
	}
	channelNames := bytes.Split(buf, []byte("\n"))

	for _, chName := range channelNames {
		go func(channelName []byte) {
			channelURL, err := getChannelURL(channelName)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[ERROR]: %v\n", err)
				return
			}
			fetchedChannel, err := fetch(channelName, channelURL)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[ERROR]: failed to fetch '%v': %v\n", string(channelName), err)
				return
			}
			err = feed.add(string(channelName), fetchedChannel)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[WARNING]: failed to subscribe to channel '%v': %v\n", string(channelName), err)
			}
			fmt.Fprintf(os.Stderr, "[INFO]: channel '%v' stored\n", string(channelName))
		}(chName)
	}
}

func notify(chName string, vid *video) error {
	notice := fmt.Sprintf("New video from %v:\n%v", chName, vid.Title)
	var args []string
	args = append(args, "-a")
	args = append(args, "notifyVid")
	args = append(args, "-t")
	args = append(args, "10000")
	args = append(args, "-u")
	args = append(args, "low")
	args = append(args, "-h")
	args = append(args, "string:x-dunst-stack-tag:ytfd")
	args = append(args, notice)
	cmd := exec.Command("dunstify", args...)
	err := cmd.Run()
	if err != nil {
		fmt.Fprintf(logTo, "[WARNING]: dunstify failed, disabling notify flag: %v\n", err)
		*flagNotify = false
		return err
	}
	return nil
}

func normalizeName(c string) string {
	c = strings.ToLower(c)
	c = strings.ReplaceAll(c, " ", "")
	return c
}

func (ldb *localDb) add(c string, ch *channel) error {
	if len(ch.Videos) == 0 {
		return fmt.Errorf("channel with no videos")
	}
	c = normalizeName(c)
	ldb.Lock()
	defer ldb.Unlock()
	ldbCh, ok := ldb.c[c]
	if ok && len(ldbCh.Videos) > 0 {
		latest := ldbCh.Videos[0]
		i := 0
		for j, vid := range ch.Videos {
			if vid.VideoId == latest.VideoId {
				i = j
				if *flagNotify && i > 0 {
					var err error
					for k := 0; k < i && err == nil; k++ {
						err = notify(c, ch.Videos[k])
					}
				}
				break
			}
		}
		ldbCh.Videos = append(ch.Videos[:i], ldbCh.Videos[:min(maxFeedSize, len(ldbCh.Videos))]...)
		return nil
	}
	ch.Videos = ch.Videos[:min(maxFeedSize, len(ch.Videos))]
	ldb.c[c] = ch
	return nil
}

func (ldb *localDb) get(c string) (*channel, error) {
	c = normalizeName(c)
	ldb.RLock()
	defer ldb.RUnlock()
	ch, ok := ldb.c[c]
	if !ok {
		return nil, fmt.Errorf("not subscribed to channel '%v'", c)
	}
	return ch, nil
}

func (ldb *localDb) rm(c string) error {
	c = normalizeName(c)
	ldb.Lock()
	defer ldb.Unlock()
	delete(ldb.c, c)
	return nil
}

func (ldb *localDb) subs() ([]*channel, error) {
	ldb.RLock()
	defer ldb.RUnlock()
	var chs []*channel
	for _, v := range ldb.c {
		chs = append(chs, v)
	}
	return chs, nil

}

func getChannelURL(bname []byte) ([]byte, error) {
	name := string(bname)
	name = strings.ReplaceAll(name, "\n", "")
	if len(name) == 0 {
		return []byte{}, fmt.Errorf("empty channel name")
	}
	resp, err := http.Get(channelURLBase + name)
	if err != nil || resp.Body == nil {
		return []byte{}, fmt.Errorf("failed to get '%v': %v", name, err)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return []byte{}, fmt.Errorf("failed to read response body: %v", err)
	}

	idRegexp := "[a-zA-Z0-9_-]{24}"
	re := regexp.MustCompile(strings.ReplaceAll(feedURLBase, "?", "\\?") + idRegexp)
	feedURLBytes := re.Find(bodyBytes)

	if len(feedURLBytes) == 0 {
		return []byte{}, fmt.Errorf("channel '%v' not found", name)
	}
	return feedURLBytes, nil
}

func fetch(chName []byte, chURL []byte) (*channel, error) {
	resp, err := http.Get(string(chURL))
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var fetchedChannel channel
	if err := xml.Unmarshal(body, &fetchedChannel); err != nil {
		return nil, err
	}
	fetchedChannel.Name = string(chName)
	var channelURL string

	channelURLElems := strings.Split(string(chURL), "=")
	if len(channelURLElems) > 0 {
		channelID := channelURLElems[1]
		channelURL = channelURLByIDBase + channelID
	} else {
		channelURL = channelURLBase + string(chName)
	}

	fetchedChannel.URL = channelURL
	if len(fetchedChannel.Videos) > maxFeedSize {
		fetchedChannel.Videos = fetchedChannel.Videos[:maxFeedSize]
	}
	return &fetchedChannel, nil
}

// send is a function that takes the state (success/failure) and response message. It encodes the response in the following way
// * first byte is the state, i.e. whether the request was successful or not.
// * second and third byte form an exponent for base 2, where 2^exponent will fit the response message
// * rest of the bytes is the response
func send(c net.Conn, st state, resp string) {
	pow := 1
	for res := 2; res < len(resp); res *= 2 {
		pow++
	}
	var msg []byte
	msg = append(msg, byte(st))
	msg = append(msg, byte(pow/10))
	msg = append(msg, byte(pow%10))
	msg = append(msg, []byte(resp)...)
	c.Write(msg)

	lines := bytes.Split(msg[3:], []byte("\n"))
	if len(lines) == 0 {
		fmt.Fprintf(logTo, "[WARNING]: sending empty response\n")
	} else {
		fmt.Fprintf(logTo, "[INFO]: sending %v bytes, '%v...'\n", len(resp), string(lines[0]))
	}
}

func handleFetch(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			fmt.Fprintf(logTo, "[ERROR]: 'fetch' handler failed to accept connection\n")
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			channelName := make([]byte, 128)
			n, err := c.Read(channelName)
			if err != nil {
				fmt.Fprintf(logTo, "[ERROR]: didn't understand '%v' for 'fetch': %v\n", string(channelName), err)
				send(c, failure, err.Error())
				return
			}
			channelName = channelName[:n]
			channelURL, err := getChannelURL(channelName)
			if err != nil {
				fmt.Fprintf(logTo, "[ERROR]: failed to get channel '%v' url: %v\n", string(channelName), err)
				send(c, failure, err.Error())
				return
			}

			channelFetched, err := fetch(channelName, channelURL)
			if err != nil {
				fmt.Fprintf(logTo, "[ERROR]: failed to fetch '%v': %v\n", string(channelName), err)
				send(c, failure, err.Error())
				return
			}
			send(c, success, channelFetched.String())
		}(conn)
	}
}

func handleAdd(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			fmt.Fprintf(logTo, "[ERROR]: 'add' handler failed to accept connection\n")
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			channelName := make([]byte, 128)
			n, err := c.Read(channelName)
			if err != nil {
				fmt.Fprintf(logTo, "[ERROR]: didn't understand '%v' for 'add': %v\n", string(channelName), err)
				send(c, failure, err.Error())
				return
			}
			channelName = channelName[:n]
			_, err = feed.get(string(channelName))
			if err == nil { // NOTE: already subbed
				send(c, failure, fmt.Sprintf("already subscribed to channel %q", string(channelName)))
				return
			}
			channelURL, err := getChannelURL(channelName)
			if err != nil {
				fmt.Fprintf(logTo, "[ERROR]: failed to get channel '%v' url: %v\n", string(channelName), err)
				send(c, failure, err.Error())
				return
			}
			channelFetched, err := fetch(channelName, channelURL)
			if err != nil {
				fmt.Fprintf(logTo, "[ERROR]: failed to fetch '%v': %v\n", string(channelName), err)
				send(c, failure, err.Error())
				return
			}
			err = feed.add(string(channelName), channelFetched)
			if err != nil {
				fmt.Fprintf(logTo, "[ERROR]: failed to add: %v\n", err)
				send(c, failure, err.Error())
				return
			}
			send(c, success, fmt.Sprintf("subscribed to channel %q", string(channelName)))
		}(conn)
	}
}

func handleGet(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			fmt.Fprintf(logTo, "[ERROR]: 'get' handler failed to accept connection\n")
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			channelName := make([]byte, 128)
			n, err := conn.Read(channelName)
			if err != nil {
				fmt.Fprintf(logTo, "[ERROR]: didn't understand '%v' for 'get': %v\n", string(channelName), err)
				send(c, failure, err.Error())
				return
			}
			channelName = channelName[:n]
			ch, err := feed.get(string(channelName))
			if err != nil {
				fmt.Fprintf(logTo, "[ERROR]: failed to get feed for channel '%v': %v\n", string(channelName), err)
				send(c, failure, err.Error())
				return
			}
			send(c, success, ch.String())
			return
		}(conn)
	}
}

func handleRm(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			fmt.Fprintf(logTo, "[ERROR]: 'rm' handler failed to accept connection\n")
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			channelName := make([]byte, 128)
			n, err := c.Read(channelName)
			if err != nil {
				fmt.Fprintf(logTo, "[ERROR]: didn't understand '%v' for 'rm': %v\n", string(channelName), err)
				send(c, failure, err.Error())
				return
			}
			channelName = channelName[:n]
			err = feed.rm(string(channelName))
			if err != nil {
				fmt.Fprintf(logTo, "[ERROR]: failed to rm channel '%v' from subscriptions: %v\n", string(channelName), err)
				send(c, failure, err.Error())
				return
			}
			send(c, success, fmt.Sprintf("unsubscribed from channel %q", string(channelName)))
			return
		}(conn)
	}
}

func refresh() {
	subs, err := feed.subs()
	if err != nil {
		fmt.Fprintf(logTo, "[ERROR]: failed to retrieve subs: %v\n", err)
		return
	}
	var wg sync.WaitGroup
	for _, sub := range subs {
		wg.Add(1)
		go func(channelName []byte, channelURL []byte) {
			defer wg.Done()
			fetchedChannel, err := fetch(channelName, channelURL)
			if err != nil {
				fmt.Fprintf(logTo, "[ERROR]: failed to fetch '%v' for refresh: %v\n", string(channelName), err)
				return
			}
			err = feed.add(string(channelName), fetchedChannel)
			if err != nil {
				fmt.Fprintf(logTo, "[WARNING]: failed to refresh channel '%v': %v\n", string(channelName), err)
				return
			}
			return
		}([]byte(sub.Name), []byte(sub.URL))
	}
	wg.Wait()
}

func handleRefresh(l net.Listener) {
	go func() {
		<-time.After(time.Duration(*flagRefreshRate) * time.Minute)
		refresh()
	}()
	for {
		conn, err := l.Accept()
		if err != nil {
			fmt.Fprintf(logTo, "[ERROR]: 'refresh' handler failed to accept connection\n")
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			refresh()
		}(conn)
	}
}

func search(query []byte) (string, error) {
	query = bytes.ReplaceAll(query, []byte(" "), []byte("+"))
	query = bytes.ToLower(query)
	resp, err := http.Get(querySearchURLBase + string(query))
	if err != nil || resp.Body == nil {
		return "", fmt.Errorf("failed to search for '%v': %v", string(query), err)
	}
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %v", err)
	}
	re := regexp.MustCompile(fmt.Sprintf("\"/@\\w*%v\\w*\"", string(query)))
	indices := re.FindAllIndex(bytes.ToLower(respBytes), -1)
	foundTheseBytesMap := make(map[string]struct{})
	for _, index := range indices {
		elem := string(respBytes[index[0]:index[1]])
		_, ok := foundTheseBytesMap[elem]
		if !ok {
			foundTheseBytesMap[elem] = struct{}{}
		}
	}
	var foundThese []string
	for k := range foundTheseBytesMap {
		foundThese = append(foundThese, k[3:len(k)-1]) // index from 3, because want to drop '"/@'; len(k)-1 to drop '"'
	}
	found := strings.Join(foundThese, ", ")
	if len(found) == 0 {
		found = fmt.Sprintf("found no channel like %q", string(query))
	}
	return found, nil
}

func handleSearch(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			fmt.Fprintf(logTo, "[ERROR]: 'search' handler failed to accept connection\n")
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			query := make([]byte, 128)
			n, err := c.Read(query)
			if err != nil {
				fmt.Fprintf(logTo, "[ERROR]: 'search' failed to read input: %v\n", err)
				send(c, failure, err.Error())
				return
			}
			query = query[:n]
			chNames, err := search(query)
			if err != nil {
				fmt.Fprintf(logTo, "[ERROR]: %v\n", err)
				send(c, failure, err.Error())
				return
			}
			send(c, success, chNames)
		}(conn)
	}
}

func handleHealth(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			fmt.Fprintf(logTo, "[ERROR]: 'health' handler failed to accept connection\n")
			break
		}
		go func(c net.Conn) {
			defer c.Close()
			buf := make([]byte, 128)
			n, err := c.Read(buf)
			if err != nil {
				fmt.Fprintf(logTo, "[WARN]: failed to read input on 'health': %v\n", err)
				send(c, failure, err.Error())
				return
			}
			buf = buf[:n]
			response := fmt.Sprintf("%q", string(buf))
			send(c, success, response)
		}(conn)
	}
}

func handleSubs(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			fmt.Fprintf(logTo, "[ERROR]: 'subs' handler failed to accept connection\n")
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			chs, err := feed.subs()
			if err != nil {
				fmt.Fprintf(logTo, "[ERROR]: failed to retrieve subs: %v\n", err)
				send(c, failure, err.Error())
				return
			}
			var channelNames []string
			for _, ch := range chs {
				channelNames = append(channelNames, ch.Name)
			}
			response := strings.Join(channelNames, "\n")
			if len(response) == 0 {
				response = "no subscriptions"
			}
			send(c, success, response)
		}(conn)
	}
}

func help() {
	fmt.Printf("%v [-notify={true|false}] [-subs=/path/to/subs/file] [-refrate={minutes}] [-debug]\n", os.Args[0])
	flag.PrintDefaults()
	fmt.Printf("\nExamples:\n")
	fmt.Printf("\t* %v -help\n", os.Args[0])
	fmt.Printf("\t* %v -subs=./example.subs -refrate=7 -notify\n", os.Args[0])
	fmt.Printf("\t* %v -subs=./example.subs -refrate=7 -notify=false -debug\n", os.Args[0])
}

func main() {
	flagNotify = flag.Bool("notify", true, "creates dunstify notification when a new video for a subscribed channel is detected. Depends on dunstify. If dunstify is not detected in the system, internal flag value is set to false")
	flagSubsFromFile := flag.String("subs", "", "path to file that contains names of subscribed channels, one per each line")
	flagRefreshRate = flag.Int("refrate", 15, "refresh rate in minutes, i.e. how often daemon checks youtube")
	flagHelp := flag.Bool("help", false, "print help")
	flagDebug := flag.Bool("debug", false, "sets logging to stderr")
	flag.Parse()

	if *flagHelp {
		help()
		return
	}

	if *flagDebug {
		logTo = os.Stderr
	} else {
		var err error
		logTo, err = os.OpenFile("/tmp/ytfd.log", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR]: failed to open log file: %v\n", err)
			return
		}
		defer logTo.Close()
	}

	// gracefully close on exit
	defer listeners.close()
	sigtermCh := make(chan os.Signal)
	signal.Notify(sigtermCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigtermCh
		listeners.close()
		os.Exit(0)
	}()

	// feat: subs from file
	go subsFromFile(*flagSubsFromFile)

	endpoints := [listenersSize]endpoint{
		{
			sockName: "/tmp/ytfd.fetch.sock",
			handle:   handleFetch,
		},
		{
			sockName: "/tmp/ytfd.add.sock",
			handle:   handleAdd,
		},
		{
			sockName: "/tmp/ytfd.get.sock",
			handle:   handleGet,
		},
		{
			sockName: "/tmp/ytfd.rm.sock",
			handle:   handleRm,
		},
		{
			sockName: "/tmp/ytfd.refresh.sock",
			handle:   handleRefresh,
		},
		{
			sockName: "/tmp/ytfd.search.sock",
			handle:   handleSearch,
		},
		{
			sockName: "/tmp/ytfd.subs.sock",
			handle:   handleSubs,
		},
		{
			sockName: "/tmp/ytfd.health.sock",
			handle:   handleHealth,
			blocking: true,
		},
	}

	for _, endpoint := range endpoints {
		endpoint.serve()
	}
}
