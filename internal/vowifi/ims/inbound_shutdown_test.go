package ims

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestInboundRegistrationAfterCloseSnapshotIsRejected(t *testing.T) {
	session := &Session{}
	session.closeInboundConnections()
	accepted, peer := net.Pipe()
	defer accepted.Close()
	defer peer.Close()
	if session.trackInboundConnection(accepted) {
		t.Fatal("socket escaped shutdown snapshot")
	}
	if len(session.inboundConnections) != 0 {
		t.Fatal("late socket remained tracked")
	}
}

func TestInboundRegisteredBeforeCloseIsClosedAndJoined(t *testing.T) {
	session := &Session{}
	accepted, peer := net.Pipe()
	defer peer.Close()
	if !session.trackInboundConnection(accepted) {
		t.Fatal("live connection rejected")
	}
	go func() { defer session.receiveDone.Done(); _, _ = accepted.Read(make([]byte, 1)) }()
	session.closeInboundConnections()
	done := make(chan struct{})
	go func() { session.receiveDone.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown stranded an accepted reader")
	}
	if _, err := peer.Write([]byte{1}); err != io.ErrClosedPipe {
		t.Fatal("accepted socket remained open", err)
	}
}

func TestInboundConcurrentAdmissionAndShutdownNeverStrandsSocket(t *testing.T) {
	for repeat := 0; repeat < 100; repeat++ {
		ctx, cancel := context.WithCancel(context.Background())
		session := &Session{refreshContext: ctx}
		session.receiveDone.Add(1) // the live accept loop reference
		accepted, peer := net.Pipe()
		var workers sync.WaitGroup
		workers.Add(2)
		start := make(chan struct{})
		go func() {
			defer workers.Done()
			defer session.receiveDone.Done()
			<-start
			if session.trackInboundConnection(accepted) {
				go func() { defer session.receiveDone.Done(); _, _ = accepted.Read(make([]byte, 1)) }()
			} else {
				accepted.Close()
			}
		}()
		go func() { defer workers.Done(); <-start; cancel(); session.closeInboundConnections() }()
		close(start)
		workers.Wait()
		done := make(chan struct{})
		go func() { session.receiveDone.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("admission passed shutdown fence")
		}
		peer.Close()
	}
}
