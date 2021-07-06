package worker

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"devcircus.com/cli/swd/pkg/logger"

	"github.com/endeveit/go-snippets/cli"
	"github.com/endeveit/go-snippets/config"
	log "github.com/sirupsen/logrus"
)

// TypeRemote remote selector
const TypeRemote = "remote"

// TypeBackup backup selector
const TypeBackup = "backup"

// Host information
type Host struct {
	host        string
	isBroken    bool
	brokenSince time.Time
}

// Worker data structure
type Worker struct {
	rwMutext sync.RWMutex
	remote   []Host
	backup   []Host
	client   http.Client
	once     sync.Once
	timeout  time.Duration
}

var (
	errFoo error = fmt.Errorf("Response code is not 2xx")
)

// NewWorker create a new instance worker
func NewWorker() *Worker {
	w := Worker{}

	return &w
}

func (w *Worker) init() {
	w.once.Do(func() {
		var (
			r, b, t, host string
			err           error
			brokenTimeout time.Duration
		)

		r, err = config.Instance().String("remote", "hosts")
		cli.CheckError(err)

		b, err = config.Instance().String("remote", "backup")
		cli.CheckError(err)

		t, err = config.Instance().String("remote", "timeout")
		if err != nil {
			t = "1s"
		}

		w.timeout, err = time.ParseDuration(t)
		cli.CheckError(err)

		t, err = config.Instance().String("remote", "broken_host_offline")
		if err != nil {
			t = "60s"
		}
		brokenTimeout, err = time.ParseDuration(t)
		cli.CheckError(err)

		for _, host = range stringSliceUnique(strings.Split(r, ";")) {
			h := Host{
				host:     host,
				isBroken: false,
			}

			w.remote = append(w.remote, h)
		}

		for _, host = range stringSliceUnique(strings.Split(b, ";")) {
			h := Host{
				host:     host,
				isBroken: false,
			}

			w.backup = append(w.backup, h)
		}

		w.client = http.Client{
			Timeout: w.timeout,
		}

		// Each 1 second check if broken timeout exceeded
		go func() {
			var h Host

			for _, h = range w.remote {
				if h.isBroken && time.Since(h.brokenSince) > brokenTimeout {
					h.isBroken = false
				}
			}

			for _, h = range w.backup {
				if h.isBroken && time.Since(h.brokenSince) > brokenTimeout {
					h.isBroken = false
				}
			}

			time.Sleep(1 * time.Second)
		}()
	})
}

// Do job
func (w *Worker) Do(r *http.Request) *http.Response {
	w.init()

	response := w.doUpstreams(r)

	if response == nil {
		response = w.doBackups(r)
	}

	return response
}

// Request upstreams in parallel
func (w *Worker) doUpstreams(r *http.Request) *http.Response {
	var response *http.Response

	resChan := make(chan *http.Response)
	hosts := w.getAliveHosts(w.remote)

	if len(hosts) == 0 {
		return nil
	}

	for i, host := range hosts {
		go func(id int, h Host, c chan *http.Response) {
			var req *http.Request

			if req = w.requestSetHost(h, r); req != nil {
				res, err := w.makeRequest(req)
				c <- res

				if err != nil && err != errFoo {
					w.markHostBroken(TypeRemote, id)
				}

				return
			}
			c <- nil

			return

		}(i, host, resChan)
	}

	for i := 0; i < len(hosts); i++ {
		select {
		case r := <-resChan:
			if response == nil && r != nil {
				response = r
			}
		}
	}

	return response
}

// Request backups in serial
func (w *Worker) doBackups(r *http.Request) *http.Response {
	hosts := w.getAliveHosts(w.backup)
	if len(hosts) == 0 {
		return nil
	}

	for id, host := range hosts {
		var req *http.Request

		if req = w.requestSetHost(host, r); req != nil {
			res, err := w.makeRequest(req)

			if err != nil && err != errFoo {
				w.markHostBroken(TypeBackup, id)
			}

			return res
		}
		return nil
	}

	return nil
}

func (w *Worker) makeRequest(req *http.Request) (*http.Response, error) {
	resp, err := w.client.Do(req)

	if resp != nil && resp.StatusCode >= 400 {
		defer resp.Body.Close()
	}

	if err == nil && resp.StatusCode < 400 {
		logger.Instance().WithFields(log.Fields{
			"url":    resp.Request.URL,
			"status": resp.StatusCode,
		}).Debug("Request success")

		return resp, nil
	}

	if err == nil {
		err = errFoo
	}

	logger.Instance().WithFields(log.Fields{
		"url":   req.URL.String(),
		"error": err,
	}).Error("Request error")

	return nil, err
}

func (w *Worker) requestSetHost(h Host, r *http.Request) *http.Request {
	u, err := url.Parse(h.host)
	if err != nil {
		logger.Instance().WithFields(log.Fields{
			"url":   h,
			"error": err,
		}).Error("Remote parse error")

		return nil
	}

	req := r
	req.URL.Host = u.Host
	req.URL.Scheme = u.Scheme

	return req
}

func (w *Worker) markHostBroken(hostType string, hostID int) {
	switch hostType {
	case TypeRemote:
		if w.remote[hostID].isBroken == false {
			host := w.remote[hostID]
			host.isBroken = true
			host.brokenSince = time.Now()

			w.rwMutext.Lock()
			w.remote[hostID] = host
			w.rwMutext.Unlock()

			logger.Instance().WithFields(log.Fields{
				"host": host.String(),
			}).Error("Remote host marked broken")
		}
	case TypeBackup:
		if w.backup[hostID].isBroken == false {
			host := w.backup[hostID]
			host.isBroken = true
			host.brokenSince = time.Now()

			w.rwMutext.Lock()
			w.backup[hostID] = host
			w.rwMutext.Unlock()

			logger.Instance().WithFields(log.Fields{
				"host": host.String(),
			}).Error("Backup host marked broken")
		}
	}
}

func (w *Worker) getAliveHosts(hosts []Host) (result []Host) {
	for _, host := range hosts {
		if host.isBroken == false {
			result = append(result, host)
		}
	}

	return result
}

func stringSliceUnique(slice []string) (result []string) {
	m := make(map[string]bool)

	for _, v := range slice {
		m[v] = true
	}

	for k := range m {
		result = append(result, k)
	}

	return
}

func (h *Host) String() string {
	return fmt.Sprintf("host=%s broken=%t since=%s", h.host, h.isBroken, h.brokenSince.Format(time.RFC3339))
}
