package fronted

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/eventual"
	"github.com/getlantern/golog"
	"github.com/getlantern/idletiming"
	"github.com/getlantern/netx"
	"github.com/getlantern/tlsdialer"
)

const (
	numberToVetInitially       = 1000
	defaultMaxAllowedCachedAge = 24 * time.Hour
	defaultMaxCacheSize        = 1000
	defaultCacheSaveInterval   = 5 * time.Second
	maxTries                   = 10000 // 6
	headTestURL                = "http://dlymairwlc89h.cloudfront.net/index.html"
)

var (
	log       = golog.LoggerFor("fronted")
	_instance = eventual.NewValue()

	// Shared client session cache for all connections
	clientSessionCache = tls.NewLRUClientSessionCache(1000)
)

// direct is an implementation of http.RoundTripper
type direct struct {
	tlsConfigsMutex     sync.Mutex
	tlsConfigs          map[string]*tls.Config
	certPool            *x509.CertPool
	candidates          chan *Masquerade
	masquerades         chan *Masquerade
	maxAllowedCachedAge time.Duration
	maxCacheSize        int
	cacheSaveInterval   time.Duration
	toCache             chan *Masquerade
}

// Configure sets the masquerades to use, the trusted root CAs, and the
// cache file for caching masquerades to set up direct domain fronting.
func Configure(pool *x509.CertPool, masquerades map[string][]*Masquerade, cacheFile string) {
	log.Trace("Configuring fronted")
	if masquerades == nil || len(masquerades) == 0 {
		log.Errorf("No masquerades!!")
		return
	}

	CloseCache()

	// Make a copy of the masquerades to avoid data races.
	size := 0
	for _, v := range masquerades {
		size += len(v)
	}

	if size == 0 {
		log.Errorf("No masquerades!!")
		return
	}

	d := &direct{
		tlsConfigs:          make(map[string]*tls.Config),
		certPool:            pool,
		candidates:          make(chan *Masquerade, size),
		masquerades:         make(chan *Masquerade, size),
		maxAllowedCachedAge: defaultMaxAllowedCachedAge,
		maxCacheSize:        defaultMaxCacheSize,
		cacheSaveInterval:   defaultCacheSaveInterval,
		toCache:             make(chan *Masquerade, defaultMaxCacheSize),
	}

	numberToVet := numberToVetInitially
	if cacheFile != "" {
		numberToVet -= d.initCaching(cacheFile)
	}

	d.loadCandidates(masquerades)
	if numberToVet > 0 {
		d.vetInitial(numberToVet)
	} else {
		log.Debug("Not vetting any masquerades because we have enough cached ones")
	}
	_instance.Set(d)
}

func (d *direct) loadCandidates(initial map[string][]*Masquerade) {
	log.Debug("Loading candidates")
	for key, arr := range initial {
		size := len(arr)
		log.Tracef("Adding %d candidates for %v", size, key)
		for i := 0; i < size; i++ {
			// choose index uniformly in [i, n-1]
			r := i + rand.Intn(size-i)
			log.Trace("Adding candidate")
			d.candidates <- arr[r]
		}
	}
}

func (d *direct) vetInitial(numberToVet int) {
	log.Tracef("Vetting %d initial candidates in parallel", numberToVet)
	for i := 0; i < numberToVet; i++ {
		go func() {
			for {
				more := d.vetOne()
				if !more {
					return
				}
			}
		}()
	}
}

func (d *direct) vetOne() bool {
	// We're just testing the ability to connect here, destination site doesn't
	// really matter
	log.Trace("Vetting one")
	conn, keepMasquerade, masqueradesRemain, err := d.dialWith(d.candidates)
	if err != nil {
		return masqueradesRemain
	}
	defer conn.Close()

	// Do a HEAD request to verify that domain-fronting works
	client := &http.Client{
		Transport: httpTransport(conn, nil),
	}
	resp, err := client.Head(headTestURL)
	if err != nil {
		log.Tracef("Unsuccessful vetting with HEAD request, discarding masquerade")
		return masqueradesRemain
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Tracef("Unexpected response status vetting masquerade: %v, %v", resp.StatusCode, resp.Status)
		return masqueradesRemain
	}
	log.Trace("Finished vetting one")
	keepMasquerade()
	return false
}

// NewDirect creates a new http.RoundTripper that does direct domain fronting.
func NewDirect(timeout time.Duration) http.RoundTripper {
	instance, ok := _instance.Get(timeout)
	if !ok {
		panic(fmt.Errorf("No DirectHttpClient available within %v", timeout))
	}
	return instance.(http.RoundTripper)
}

// Do continually retries a given request until it succeeds because some
// fronting providers will return a 403 for some domains.
func (d *direct) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	var err error
	if req.Body != nil {
		// store body in-memory to be able to replay it if necessary
		body, err = ioutil.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("Unable to read request body: %v", err)
		}
	}
	for i := 0; i < maxTries; i++ {
		log.Debug("Trying")
		if body != nil {
			req.Body = ioutil.NopCloser(bytes.NewReader(body))
		}
		conn, keepMasquerade, err := d.dial()
		if err != nil {
			// unable to find good masquerade, fail
			return nil, err
		}
		tr := httpTransport(conn, clientSessionCache)
		resp, err := tr.RoundTrip(req)
		if err != nil {
			log.Errorf("Could not complete request %v", err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode > 199 && resp.StatusCode < 400 {
			keepMasquerade()
			return resp, nil
		}
		log.Debug(resp.StatusCode)
	}

	return nil, errors.New("Could not complete request even with retries")
}

// Dial dials out using a masquerade. If the available masquerade fails, it
// retries with others until it either succeeds or exhausts the available
// masquerades. If successful, it returns a function that the caller can use to
// keep the masquerade (i.e. if masquerade was good, keep it).
func (d *direct) dial() (net.Conn, func(), error) {
	conn, keepMasquerade, _, err := d.dialWith(d.masquerades)
	return conn, keepMasquerade, err
}

func (d *direct) dialWith(in chan *Masquerade) (net.Conn, func(), bool, error) {
	retryLater := make([]*Masquerade, 0)
	defer func() {
		for _, m := range retryLater {
			in <- m
		}
	}()

	for {
		var m *Masquerade
		select {
		case m = <-in:
			log.Trace("Got vetted masquerade")
		default:
			log.Trace("No vetted masquerade found, falling back to unvetted candidate")
			select {
			case m = <-d.candidates:
				log.Trace("Got unvetted masquerade")
			default:
				return nil, nil, false, errors.New("Could not dial any masquerade?")
			}
		}

		log.Tracef("Dialing to %v", m)

		// We do the full TLS connection here because in practice the domains at a given IP
		// address can change frequently on CDNs, so the certificate may not match what
		// we expect.
		if conn, err := d.dialServerWith(m); err != nil {
			log.Tracef("Could not dial to %v, %v", m.IpAddress, err)
			// Don't re-add this candidate if it's any certificate error, as that
			// will just keep failing and will waste connections. We can't access the underlying
			// error at this point so just look for "certificate" and "handshake".
			if strings.Contains(err.Error(), "certificate") || strings.Contains(err.Error(), "handshake") {
				log.Tracef("Not re-adding candidate that failed on error '%v'", err.Error())
			} else {
				log.Tracef("Unexpected error dialing, keeping masquerade: %v", err)
				retryLater = append(retryLater, m)
			}
		} else {
			log.Tracef("Got successful connection to: %v", m)
			idleTimeout := 70 * time.Second

			log.Trace("Wrapping connecting in idletiming connection")
			conn = idletiming.Conn(conn, idleTimeout, func() {
				log.Tracef("Connection to %v idle for %v, closed", conn.RemoteAddr(), idleTimeout)
			})
			log.Trace("Returning connection")
			keepMasquerade := func() {
				// Requeue the working connection to masquerades
				d.masquerades <- m
				m.LastVetted = time.Now()
				select {
				case d.toCache <- m:
					// ok
				default:
					// cache writing has fallen behind, drop masquerade
				}
			}
			return conn, keepMasquerade, true, nil
		}
	}
}

func (d *direct) dialServerWith(masquerade *Masquerade) (net.Conn, error) {
	tlsConfig := d.tlsConfig(masquerade)
	dialTimeout := 10 * time.Second
	sendServerNameExtension := false

	conn, err := tlsdialer.DialTimeout(
		netx.DialTimeout,
		dialTimeout,
		"tcp",
		masquerade.IpAddress+":443",
		sendServerNameExtension, // SNI or no
		tlsConfig)

	if err != nil && masquerade != nil {
		err = fmt.Errorf("Unable to dial masquerade %s: %s", masquerade.Domain, err)
	}
	return conn, err
}

// tlsConfig builds a tls.Config for dialing the upstream host. Constructed
// tls.Configs are cached on a per-masquerade basis to enable client session
// caching and reduce the amount of PEM certificate parsing.
func (d *direct) tlsConfig(m *Masquerade) *tls.Config {
	d.tlsConfigsMutex.Lock()
	defer d.tlsConfigsMutex.Unlock()

	tlsConfig := d.tlsConfigs[m.Domain]
	if tlsConfig == nil {
		tlsConfig = &tls.Config{
			ClientSessionCache: tls.NewLRUClientSessionCache(1000),
			InsecureSkipVerify: false,
			ServerName:         m.Domain,
			RootCAs:            d.certPool,
		}
		d.tlsConfigs[m.Domain] = tlsConfig
	}

	return tlsConfig
}

func httpTransport(conn net.Conn, clientSessionCache tls.ClientSessionCache) http.RoundTripper {
	return &directTransport{
		Transport: http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				return conn, nil
			},
			TLSHandshakeTimeout: 40 * time.Second,
			DisableKeepAlives:   true,
			TLSClientConfig: &tls.Config{
				ClientSessionCache: clientSessionCache,
			},
		},
	}
}

// directTransport is a wrapper struct enabling us to modify the protocol of outgoing
// requests to make them all HTTP instead of potentially HTTPS, which breaks our particular
// implemenation of direct domain fronting.
type directTransport struct {
	http.Transport
}

func (ddf *directTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	// The connection is already encrypted by domain fronting.  We need to rewrite URLs starting
	// with "https://" to "http://", lest we get an error for doubling up on TLS.

	// The RoundTrip interface requires that we not modify the memory in the request, so we just
	// create a copy.
	norm := new(http.Request)
	*norm = *req // includes shallow copies of maps, but okay
	norm.URL = new(url.URL)
	*norm.URL = *req.URL
	norm.URL.Scheme = "http"
	return ddf.Transport.RoundTrip(norm)
}
