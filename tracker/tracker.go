package tracker

import (
	"bytes"
	bencode "code.google.com/p/bencode-go"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Tracker struct {
	Announce string
	Addr     string
	ID       string
	l        net.Listener
	m        sync.Mutex
	t        trackerTorrents
	done     chan struct{}
}

type trackerTorrents map[string]*trackerTorrent

// Single-threaded imp
type trackerTorrent struct {
	name       string
	downloaded uint64
	peers      trackerPeers
}

// key is the client's listen address, in the form IP:port
type trackerPeers map[string]*trackerPeer

type trackerPeer struct {
	listenAddr *net.TCPAddr
	id         string
	lastSeen   time.Time
	uploaded   int64
	downloaded int64
	left       int64
}

type announceParams struct {
	infoHash   string
	peerID     string
	ip         string // optional
	port       int
	uploaded   int64
	downloaded int64
	left       int64
	compact    bool
	noPeerID   bool
	event      string
	numWant    int
	trackerID  string
}

type bmap map[string]interface{}

func getBool(v url.Values, key string) (b bool, err error) {
	val := v.Get(key)
	if val == "" {
		err = fmt.Errorf("Missing query parameter: %v", key)
		return
	}
	return strconv.ParseBool(val)
}

func getInt64(v url.Values, key string) (i int64, err error) {
	val := v.Get(key)
	if val == "" {
		err = fmt.Errorf("Missing query parameter: %v", key)
		return
	}
	return strconv.ParseInt(val, 10, 64)
}

func getInt(v url.Values, key string) (i int, err error) {
	var i64 int64
	i64, err = getInt64(v, key)
	if err != nil {
		return
	}
	i = int(i64)
	return
}

func (a *announceParams) parse(u *url.URL) (err error) {
	q := u.Query()
	a.infoHash = q.Get("info_hash")
	if a.infoHash == "" {
		err = fmt.Errorf("Missing info_hash")
		return
	}
	a.ip = q.Get("ip")
	a.peerID = q.Get("peer_id")
	a.port, err = getInt(q, "port")
	if err != nil {
		return
	}
	a.uploaded, err = getInt64(q, "uploaded")
	if err != nil {
		return
	}
	a.downloaded, err = getInt64(q, "downloaded")
	if err != nil {
		return
	}
	a.left, err = getInt64(q, "left")
	if err != nil {
		return
	}
	if q.Get("compact") != "" {
		a.compact, err = getBool(q, "compact")
		if err != nil {
			return
		}
	}
	if q.Get("no_peer_id") != "" {
		a.noPeerID, err = getBool(q, "no_peer_id")
		if err != nil {
			return
		}
	}
	a.event = q.Get("event")
	if numWant := q.Get("numwant"); numWant != "" {
		a.numWant, err = getInt(q, numWant)
		if err != nil {
			return
		}
	}
	a.trackerID = q.Get("trackerid")
	return
}

func randomHexString(n int) string {
	return randomString("0123456789abcdef", n)
}

func randomString(s string, n int) string {
	b := make([]byte, n)
	slen := len(s)
	for i := 0; i < n; i++ {
		b[i] = b[rand.Intn(slen)]
	}
	return string(b)
}

func newTrackerPeerListenAddress(requestRemoteAddr string, params *announceParams) (addr *net.TCPAddr, err error) {
	var host string
	if params.ip != "" {
		host = params.ip
	} else {
		host, _, err = net.SplitHostPort(requestRemoteAddr)
		if err != nil {
			return
		}
	}
	return net.ResolveTCPAddr("tcp", net.JoinHostPort(host, strconv.Itoa(params.port)))
}

func NewTracker() *Tracker {
	return &Tracker{t: NewTrackerTorrents()}
}

func (t *Tracker) ListenAndServe() (err error) {
	t.done = make(chan struct{})
	if t.ID == "" {
		t.ID = randomHexString(20)
	}
	addr := t.Addr
	if addr == "" {
		addr = ":80"
	}
	t.l, err = net.Listen("tcp", addr)
	if err != nil {
		return
	}
	serveMux := http.NewServeMux()
	announce := t.Announce
	if announce == "" {
		announce = "/"
	}
	serveMux.HandleFunc(announce, t.handleAnnounce)
	scrapeURL := ScrapeURL(announce)
	if scrapeURL != "" {
		serveMux.HandleFunc(scrapeURL, t.handleScrape)
	}
	err = http.Serve(t.l, serveMux)
	go t.reaper()
	return
}

func ScrapeURL(announceURL string) string {
	lastSlashIndex := strings.LastIndex(announceURL, "/")
	if lastSlashIndex >= 0 {
		firstPart := announceURL[0 : lastSlashIndex+1]
		lastPart := announceURL[lastSlashIndex+1:]
		announce := "announce"
		if strings.HasPrefix(lastPart, announce) {
			afterAnnounce := lastPart[len(announce):]
			return strings.Join([]string{firstPart, "scrape", afterAnnounce}, "")
		}
	}
	return ""
}

func (t *Tracker) handleAnnounce(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	var response bmap = make(bmap)
	var params announceParams
	var peerListenAddress *net.TCPAddr
	err := params.parse(r.URL)
	if err == nil {
		if params.trackerID != "" && params.trackerID != t.ID {
			err = fmt.Errorf("Incorrect tracker ID: %#v", params.trackerID)
		}
	}
	if err == nil {
		peerListenAddress, err = newTrackerPeerListenAddress(r.RemoteAddr, &params)
	}
	if err == nil {
		now := time.Now()
		t.m.Lock()
		err = t.t.handleAnnounce(now, peerListenAddress, &params, response)
		t.m.Unlock()
		if err == nil {
			response["interval"] = int64(30 * 60)
			response["tracker id"] = t.ID
		}
	}
	var b bytes.Buffer
	if err != nil {
		log.Printf("announce from %v failed: %#v", r.RemoteAddr, err.Error())
		var errorResponse bmap = make(bmap)
		errorResponse["failure reason"] = err.Error()
		err = bencode.Marshal(&b, errorResponse)
	} else {
		err = bencode.Marshal(&b, response)
	}
	if err == nil {
		w.Write(b.Bytes())
	}
}

func (t *Tracker) handleScrape(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
}

func (t *Tracker) Quit() (err error) {
	t.l.Close()
	close(t.done)
	return
}

func (t *Tracker) Register(infoHash, name string) (err error) {
	log.Printf("Register(%#v,%#v)", infoHash, name)
	t.m.Lock()
	defer t.m.Unlock()
	err = t.t.register(infoHash, name)
	return
}

func (t *Tracker) Unregister(infoHash string) (err error) {
	t.m.Lock()
	defer t.m.Unlock()
	err = t.t.unregister(infoHash)
	return
}

func (t *Tracker) reaper() {
	checkDuration := 30 * time.Minute
	reapDuration := 2 * checkDuration
	ticker := time.Tick(checkDuration)
	select {
	case <-t.done:
		return
	case <-ticker:
		t.m.Lock()
		defer t.m.Unlock()
		deadline := time.Now().Add(-reapDuration)
		t.t.reap(deadline)
	}
}

func NewTrackerTorrents() trackerTorrents {
	return make(trackerTorrents)
}

func (t trackerTorrents) handleAnnounce(now time.Time, peerListenAddress *net.TCPAddr, params *announceParams, response bmap) (err error) {
	if tt, ok := t[params.infoHash]; ok {
		err = tt.handleAnnounce(now, peerListenAddress, params, response)
	} else {
		err = fmt.Errorf("Unknown infoHash %#v", params.infoHash)
		return
	}
	return
}

func (t trackerTorrents) register(infoHash, name string) (err error) {
	if t2, ok := t[infoHash]; ok {
		err = fmt.Errorf("Already have a torrent %#v with infoHash %v", t2.name, infoHash)
		return
	}
	t[infoHash] = &trackerTorrent{name: name, peers: make(trackerPeers)}
	return
}

func (t trackerTorrents) unregister(infoHash string) (err error) {
	delete(t, infoHash)
	return
}

func (t *trackerTorrent) countPeers() (complete, incomplete int) {
	for _, p := range t.peers {
		if p.isComplete() {
			complete++
		} else {
			incomplete++
		}
	}
	return
}

func (t *trackerTorrent) handleAnnounce(now time.Time, peerListenAddress *net.TCPAddr, params *announceParams, response bmap) (err error) {
	peerKey := peerListenAddress.String()
	var peer *trackerPeer
	var ok bool
	if peer, ok = t.peers[peerKey]; ok {
		// Does the new peer match the old peer?
		if peer.id != params.peerID {
			log.Printf("Peer changed ID. %#v != %#v", peer.id, params.peerID)
			delete(t.peers, peerKey)
			peer = nil
		}
	}
	if peer == nil {
		peer = &trackerPeer{
			listenAddr: peerListenAddress,
			id:         params.peerID,
		}
		t.peers[peerKey] = peer
		log.Printf("New peer joined %#v", peer.listenAddr.String())
	}
	peer.lastSeen = now
	peer.uploaded = params.uploaded
	peer.downloaded = params.downloaded
	peer.left = params.left
	if params.event == "completed" {
		t.downloaded++
	}

	completeCount, incompleteCount := t.countPeers()
	response["complete"] = completeCount
	response["incomplete"] = incompleteCount

	peerCount := len(t.peers)
	numWant := params.numWant
	const DEFAULT_PEER_COUNT = 50
	if numWant <= 0 || numWant > DEFAULT_PEER_COUNT {
		numWant = DEFAULT_PEER_COUNT
	}
	if numWant > peerCount {
		numWant = peerCount
	}

	peerKeys := t.peers.pickRandomPeers(peerKey, params.compact, numWant)
	if params.compact {
		var b bytes.Buffer
		err = t.peers.writeCompactPeers(&b, peerKeys)
		if err != nil {
			return
		}
		response["peers"] = string(b.Bytes())
	} else {
		var peers []bmap
		noPeerID := params.noPeerID
		peers, err = t.peers.getPeers(peerKeys, noPeerID)
		if err != nil {
			return
		}
		response["peers"] = peers
	}
	return
}

func (t trackerPeers) pickRandomPeers(peerKey string, compact bool, count int) (peers []string) {
	// Cheesy approximation to picking randomly from all peers.
	// Depends upon the implementation detail that map iteration is pseudoRandom
	for k, v := range t {
		if k == peerKey {
			continue
		}
		if compact && v.listenAddr.IP.To4() == nil {
			continue
		}
		peers = append(peers, k)
		if len(peers) == count {
			break
		}
	}
	return
}

func (t trackerPeers) writeCompactPeers(b *bytes.Buffer, keys []string) (err error) {
	for _, k := range keys {
		p := t[k]
		la := p.listenAddr
		ip4 := la.IP.To4()
		if ip4 == nil {
			err = fmt.Errorf("Can't write a compact peer for a non-IPv4 peer %v %v", k, p.listenAddr.String())
			return
		}
		_, err = b.Write(ip4)
		if err != nil {
			return
		}
		port := la.Port
		portBytes := []byte{byte(port >> 8), byte(port)}
		_, err = b.Write(portBytes)
		if err != nil {
			return
		}
	}
	return
}

func (t trackerPeers) getPeers(keys []string, noPeerID bool) (peers []bmap, err error) {
	for _, k := range keys {
		p := t[k]
		la := p.listenAddr
		var peer bmap = make(bmap)
		if !noPeerID {
			peer["peer id"] = p.id
		}
		peer["ip"] = la.IP.String()
		peer["port"] = strconv.Itoa(la.Port)
		peers = append(peers, peer)
	}
	return
}

func (t trackerTorrents) reap(deadline time.Time) {
	for _, tt := range t {
		tt.reap(deadline)
	}
}

func (t *trackerTorrent) reap(deadline time.Time) {
	t.peers.reap(deadline)
}

func (t trackerPeers) reap(deadline time.Time) {
	for address, peer := range t {
		if deadline.After(peer.lastSeen) {
			log.Println("reaping", address)
			delete(t, address)
		}
	}
}

func (t *trackerPeer) isComplete() bool {
	return t.left == 0
}
