package main

import (
	"io"
	"net/http"
	"time"
)

func (d *ledgerDashboard) nextChange() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.next == nil {
		d.next = make(chan struct{})
	}
	return d.next
}

func (d *ledgerDashboard) notifyLedgerChanged() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.next != nil {
		close(d.next)
		d.next = nil
	}
}

func (d *ledgerDashboard) serveEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	next := d.nextChange()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	controller := http.NewResponseController(w)
	frame := ": connected\n\n"
	for {
		if _, err := io.WriteString(w, frame); err != nil {
			return
		}
		if err := controller.Flush(); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-next:
			next = d.nextChange()
			frame = "event: ledger-changed\ndata: {}\n\n"
		case <-heartbeat.C:
			frame = ": keepalive\n\n"
		}
	}
}
