package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	cfg := defaultConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "mail.db")
	cfg.SMTPAddr = "127.0.0.1:0"
	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.AllowedMailbox = map[string]struct{}{}
	for _, recipient := range []string{"Alice@Example.COM", "bob@example.com"} {
		normalized, err := normalizeRecipient(recipient)
		if err != nil {
			t.Fatal(err)
		}
		cfg.AllowedMailbox[normalized] = struct{}{}
	}
	cfg.MaxConnections = 8
	cfg.ReadDeadlineSec = 5
	cfg.MaxLineBytes = 2048
	cfg.MaxRecipients = 3
	cfg.MaxMessageBytes = 4096
	return cfg
}

func freeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

func startServers(t *testing.T, cfg Config) (*Store, string, string) {
	t.Helper()
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	cfg.SMTPAddr = freeAddr(t)
	smtpServer := NewSMTPServer(cfg, store, NewListenerLimiter(cfg.MaxConnections))
	go func() {
		if err := smtpServer.ListenAndServe(); err != nil && !strings.Contains(err.Error(), "closed") {
			t.Log(err)
		}
	}()

	cfg.HTTPAddr = freeAddr(t)
	httpServer := NewHTTPServer(cfg, store, NewListenerLimiter(cfg.MaxConnections))
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			t.Log(err)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	return store, cfg.SMTPAddr, cfg.HTTPAddr
}

func readReply(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	var lines []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
		if len(line) >= 4 && line[0] >= '2' && line[0] <= '5' && line[3] == ' ' {
			break
		}
	}
	return strings.Join(lines, "")
}

func TestSMTPCaptureAndHTTPRetrieval(t *testing.T) {
	cfg := testConfig(t)
	_, smtpAddr, httpAddr := startServers(t, cfg)

	conn, err := net.DialTimeout("tcp", smtpAddr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	if reply := readReply(t, reader); !strings.HasPrefix(reply, "220 ") {
		t.Fatalf("greeting = %q", reply)
	}

	send := func(chunks ...string) {
		t.Helper()
		for _, chunk := range chunks {
			if _, err := io.WriteString(conn, chunk); err != nil {
				t.Fatal(err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	expect := func(prefix string) {
		t.Helper()
		reply := readReply(t, reader)
		lines := strings.Split(strings.TrimSuffix(reply, "\r\n"), "\r\n")
		last := lines[len(lines)-1]
		if !strings.HasPrefix(last, prefix+" ") {
			t.Fatalf("want %s, got %q", prefix, reply)
		}
	}

	send("EH", "LO ex", "ample.com\r\n", "MAIL FROM:<>", "\r\nRCPT TO:<Alice@example.com>\r\n")
	expect("250")
	expect("250")
	reply := readReply(t, reader)
	if !strings.HasPrefix(reply, "250 ") {
		t.Fatalf("first RCPT = %q", reply)
	}
	send("RCPT TO:<bob@EXAMPLE.COM>\r\nRCPT TO:<bad@example.net>\r\nDATA\r\n")
	expect("250")
	expect("550")
	expect("354")

	raw := "Subject: test\r\n\r\n.escaped line\r\nsecond line\r\n.\r\n"
	if _, err := io.WriteString(conn, raw); err != nil {
		t.Fatal(err)
	}
	reply = readReply(t, reader)
	if !strings.HasPrefix(reply, "250 OK: queued as ") {
		t.Fatalf("save reply = %q", reply)
	}
	id := strings.TrimSuffix(strings.TrimPrefix(reply, "250 OK: queued as "), "\r\n")

	listResponse, err := http.Get("http://" + httpAddr + "/messages?recipient=Alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(listResponse.Body)
	listResponse.Body.Close()
	if listResponse.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d: %s", listResponse.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(id)) || !bytes.Contains(body, []byte(`"sender":""`)) {
		t.Fatalf("list body = %s", body)
	}

	messageResponse, err := http.Get("http://" + httpAddr + "/messages/" + id)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(messageResponse.Body)
	messageResponse.Body.Close()
	if messageResponse.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"recipients":["Alice@example.com","bob@example.com"]`)) {
		t.Fatalf("message body = %s", body)
	}

	rawResponse, err := http.Get("http://" + httpAddr + "/messages/" + id + "/raw")
	if err != nil {
		t.Fatal(err)
	}
	defer rawResponse.Body.Close()
	downloaded, _ := io.ReadAll(rawResponse.Body)
	want := "Subject: test\r\n\r\nescaped line\r\nsecond line\r\n"
	if rawResponse.StatusCode != http.StatusOK || string(downloaded) != want {
		t.Fatalf("raw status=%d body=%q", rawResponse.StatusCode, downloaded)
	}
	if contentType := rawResponse.Header.Get("Content-Type"); contentType != "message/rfc822" {
		t.Fatalf("content type = %q", contentType)
	}
}

func TestSMTPRejectsBareLFAndUnknownRecipient(t *testing.T) {
	cfg := testConfig(t)
	_, smtpAddr, _ := startServers(t, cfg)
	conn, err := net.Dial("tcp", smtpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	readReply(t, reader)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	io.WriteString(conn, "EHLO example.com\n")
	for {
		_, err := reader.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("bare LF error = %v", err)
		}
	}

	conn2, err := net.Dial("tcp", smtpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	reader2 := bufio.NewReader(conn2)
	readReply(t, reader2)
	conn2.SetReadDeadline(time.Now().Add(2 * time.Second))
	io.WriteString(conn2, "EHLO example.com\r\nMAIL FROM:<>")
	reply := readReply(t, reader2)
	if !strings.HasPrefix(strings.SplitAfter(reply, "\r\n")[0], "250-") {
		t.Fatalf("EHLO reply = %q", reply)
	}
	io.WriteString(conn2, "\r\nRCPT TO:<unknown@example.com>\r\n")
	mailReply := readReply(t, reader2)
	if !strings.HasPrefix(mailReply, "250 ") {
		t.Fatalf("MAIL reply = %q", mailReply)
	}
	reply = readReply(t, reader2)
	if !strings.HasPrefix(reply, "550 ") {
		t.Fatalf("unknown recipient reply = %q", reply)
	}
}

func TestHalfMessageIsNotSaved(t *testing.T) {
	cfg := testConfig(t)
	store, smtpAddr, _ := startServers(t, cfg)
	conn, err := net.Dial("tcp", smtpAddr)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	readReply(t, reader)
	io.WriteString(conn, "HELO example.com\r\nMAIL FROM:<>")
	readReply(t, reader)
	io.WriteString(conn, "RCPT TO:<bob@example.com>\r\nDATA\r\n")
	readReply(t, reader)
	readReply(t, reader)
	io.WriteString(conn, "Subject: incomplete\r\n")
	conn.Close()
	time.Sleep(50 * time.Millisecond)

	var count int
	if err := store.db.QueryRow("SELECT count(*) FROM messages").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("saved %d partial messages", count)
	}
}

func TestRSETClearsEnvelopeAndCompletedTransactionStartsNext(t *testing.T) {
	cfg := testConfig(t)
	_, smtpAddr, _ := startServers(t, cfg)
	conn, err := net.Dial("tcp", smtpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	readReply(t, reader)
	commands := "EHLO example.com\r\n" +
		"MAIL FROM:<first@example.net>\r\n" +
		"RCPT TO:<bob@example.com>\r\n" +
		"RSET\r\n" +
		"DATA\r\n" +
		"MAIL FROM:<second@example.net>\r\n" +
		"RCPT TO:<bob@example.com>\r\n" +
		"DATA\r\n" +
		"Subject: second\r\n\r\nbody\r\n.\r\n" +
		"MAIL FROM:<third@example.net>\r\n" +
		"RCPT TO:<bob@example.com>\r\n" +
		"DATA\r\n" +
		"Subject: third\r\n\r\nbody\r\n.\r\n" +
		"QUIT\r\n"
	io.WriteString(conn, commands)
	wantCodes := []string{"250", "250", "250", "250", "503", "250", "250", "354", "250", "250", "250", "354", "250", "221"}
	for _, want := range wantCodes {
		reply := readReply(t, reader)
		lines := strings.Split(strings.TrimSuffix(reply, "\r\n"), "\r\n")
		if got := lines[len(lines)-1][:3]; got != want {
			t.Fatalf("want %s, got %q", want, reply)
		}
	}
}

func TestOversizedMessageIsRejectedAndNotSaved(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxMessageBytes = 12
	store, smtpAddr, _ := startServers(t, cfg)
	conn, err := net.Dial("tcp", smtpAddr)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	readReply(t, reader)
	io.WriteString(conn, "EHLO example.com\r\nMAIL FROM:<>")
	readReply(t, reader)
	io.WriteString(conn, "\r\nRCPT TO:<bob@example.com>\r\nDATA\r\n")
	readReply(t, reader)
	readReply(t, reader)
	reply := readReply(t, reader)
	if !strings.HasPrefix(reply, "354 ") {
		t.Fatalf("DATA reply = %q", reply)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	io.WriteString(conn, "Subject: long\r\n\r\n12345\r\n")
	reply, _ = reader.ReadString('\n')
	if !strings.HasPrefix(reply, "552 ") {
		t.Fatalf("oversize reply = %q", reply)
	}
	conn.Close()

	var count int
	if err := store.db.QueryRow("SELECT count(*) FROM messages").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("saved %d oversized messages", count)
	}
}

func TestInvalidHTTPQuery(t *testing.T) {
	cfg := testConfig(t)
	_, _, httpAddr := startServers(t, cfg)
	response, err := http.Get("http://" + httpAddr + "/messages?recipient=bad")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", response.StatusCode)
	}

	response.Body.Close()
	response, err = http.Get("http://" + httpAddr + "/messages?recipient=unknown@example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown recipient status = %d", response.StatusCode)
	}
}

func TestNormalizeRecipient(t *testing.T) {
	address, err := normalizeRecipient("User.Name@EXAMPLE.com")
	if err != nil || address != "User.Name@example.com" {
		t.Fatalf("address=%q err=%v", address, err)
	}
	for _, invalid := range []string{".a@example.com", "a.@example.com", "a..b@example.com", "a@com", "a@-example.com", "a b@example.com"} {
		if _, err := normalizeRecipient(invalid); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

func TestStorePersistsAfterReopen(t *testing.T) {
	cfg := testConfig(t)
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.SaveMessage("", []string{"bob@example.com"}, []byte("Subject: x"+"\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	db, err := sql.Open("sqlite3", cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var saved string
	if err := db.QueryRow("SELECT raw FROM messages WHERE id = ?", id).Scan(&saved); err != nil {
		t.Fatal(err)
	}
}
