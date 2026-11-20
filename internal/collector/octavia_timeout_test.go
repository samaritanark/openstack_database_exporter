package collector

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewRegistry_OctaviaQuotaQueryTimeout(t *testing.T) {
	// Exercise Config -> registry -> Octavia registration -> quota collector
	// through the real SQL driver. Only quota queries stall; inventory queries
	// fail promptly so they cannot hide whether the quota deadline is wired.
	address := stalledQuotaServer(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const timeout = 75 * time.Millisecond
	registry := NewRegistry(Config{
		OctaviaDatabaseURL:       "mysql://test@" + address + "/octavia",
		OctaviaQuotaQueryTimeout: timeout,
	}, logger)

	start := time.Now()
	families, err := registry.Gather()
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.GreaterOrEqual(t, elapsed, timeout)
	require.Less(t, elapsed, time.Second, "configured timeout must replace the five-second default")
	found := false
	for _, family := range families {
		if family.GetName() == "openstack_loadbalancer_quota_up" {
			found = true
			require.Len(t, family.Metric, 1)
			require.EqualValues(t, 0, family.Metric[0].GetGauge().GetValue())
		} else {
			require.False(t, strings.HasPrefix(family.GetName(), "openstack_loadbalancer_quota_"), "no partial quota samples")
		}
	}
	require.True(t, found, "quota collector must be registered")
}

// stalledQuotaServer implements just the MySQL handshake, ping and query errors.
// It never executes SQL or connects to an external database.
func stalledQuotaServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer func() {
					if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
						t.Errorf("close quota connection: %v", err)
					}
				}()
				stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
				defer stop()
				// Bound a broken test too, including a lost configuration value.
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				serveStalledQuotaConnection(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		workers.Wait()
	})
	return listener.Addr().String()
}

func serveStalledQuotaConnection(conn net.Conn) {
	writePacket := func(sequence byte, payload []byte) error {
		header := []byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), sequence}
		_, err := conn.Write(append(header, payload...))
		return err
	}
	readPacket := func() ([]byte, error) {
		var header [4]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			return nil, err
		}
		size := binary.LittleEndian.Uint32(header[:]) & 0xffffff
		payload := make([]byte, size)
		_, err := io.ReadFull(conn, payload)
		return payload, err
	}
	// Protocol 10, server version, connection ID, salt, protocol-4.1 and
	// secure-connection capabilities. No TLS or authentication switch needed.
	greeting := []byte("\x0a5.7.0-test\x00\x01\x00\x00\x00abcdefgh\x00")
	greeting = append(greeting, 0x00, 0x82, 33, 2, 0, 8, 0, 21)
	greeting = append(greeting, make([]byte, 10)...)
	greeting = append(greeting, []byte("ijklmnopqrst\x00mysql_native_password\x00")...)
	if writePacket(0, greeting) != nil {
		return
	}
	if _, err := readPacket(); err != nil {
		return
	}
	ok := []byte{0, 0, 0, 2, 0, 0, 0}
	if writePacket(2, ok) != nil {
		return
	}
	for {
		packet, err := readPacket()
		if err != nil || len(packet) == 0 {
			return
		}
		switch packet[0] {
		case 0x01: // COM_QUIT
			return
		case 0x03: // COM_QUERY
			if strings.Contains(string(packet[1:]), "QuotaGetActiveLimits") || strings.Contains(string(packet[1:]), "QuotaGetAll") || strings.Contains(string(packet[1:]), "QuotaGetDefaultLimits") || strings.Contains(string(packet[1:]), "QuotaClassGetDefault") {
				// Wait for the driver to close the connection on cancellation.
				_, _ = io.Copy(io.Discard, conn)
				return
			}
			if !strings.HasPrefix(string(packet[1:]), "SET ") {
				if writePacket(1, []byte("\xff\x28\x04#42S02fixture query unavailable")) != nil {
					return
				}
				continue
			}
		}
		if writePacket(1, ok) != nil {
			return
		}
	}
}
