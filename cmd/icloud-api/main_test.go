package main

import (
	"testing"
	"time"
)

func TestHTTPWriteTimeoutCoversConfiguredSyncTimeout(t *testing.T) {
	for _, syncTimeout := range []time.Duration{10 * time.Second, 2 * time.Minute, 30 * time.Minute} {
		writeTimeout := httpWriteTimeout(syncTimeout)
		if writeTimeout <= syncTimeout {
			t.Fatalf("同步时限 %v 对应的 HTTP 写超时 = %v", syncTimeout, writeTimeout)
		}
		if writeTimeout-syncTimeout != 10*time.Second {
			t.Fatalf("HTTP 写超时余量 = %v, want %v", writeTimeout-syncTimeout, 10*time.Second)
		}
	}
}
