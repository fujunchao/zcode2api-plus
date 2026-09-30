package adminapi

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestSOCKS4ProbeUsesWorkingLine(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer upstream.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		rd := bufio.NewReader(conn)
		header := make([]byte, 8)
		if _, err := io.ReadFull(rd, header); err != nil {
			return
		}
		if header[0] != 4 || header[1] != 1 {
			return
		}
		if _, err := rd.ReadString(0); err != nil {
			return
		}
		target := net.JoinHostPort(net.IP(header[4:8]).String(), strconv.Itoa(int(binary.BigEndian.Uint16(header[2:4]))))
		remote, err := net.DialTimeout("tcp", target, time.Second)
		if err != nil {
			return
		}
		defer remote.Close()
		_, _ = conn.Write([]byte{0, 0x5a, 0, 0, 0, 0, 0, 0})
		go func() { _, _ = io.Copy(remote, rd); _ = remote.Close() }()
		_, _ = io.Copy(conn, remote)
	}()
	client, err := newProbeClient("socks4://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	client.CloseIdleConnections()
	if response.StatusCode != http.StatusOK {
		t.Fatal(response.Status)
	}
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("代理连接未释放")
	}
}
