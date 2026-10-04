package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"strings"
	"time"
)

var errCloseConnection = errors.New("smtp connection closed by policy")

const eol = "\r\n"

type SMTPServer struct {
	addr    string
	store   *Store
	cfg     Config
	limiter *ListenerLimiter
}

func NewSMTPServer(cfg Config, store *Store, limiter *ListenerLimiter) *SMTPServer {
	return &SMTPServer{addr: cfg.SMTPAddr, store: store, cfg: cfg, limiter: limiter}
}

func (s *SMTPServer) ListenAndServe() error {
	inner, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	reject := []byte("421 Too many connections, closing" + eol)
	listener := newLimitedListener(inner, s.limiter, reject)
	defer listener.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go func(conn net.Conn) {
			defer conn.Close()
			s.handle(conn)
		}(conn)
	}
}

type smtpSession struct {
	hello        bool
	senderSet    bool
	sender       string
	recipients   []string
	recipientSet map[string]struct{}
}

func newSMTPSession() *smtpSession {
	return &smtpSession{recipientSet: map[string]struct{}{}}
}

func (s *SMTPServer) handle(conn net.Conn) {
	reader := bufio.NewReaderSize(conn, s.cfg.MaxLineBytes+1)
	session := newSMTPSession()
	if !s.writeReply(conn, "220 localhost SMTP capture ready") {
		return
	}
	for {
		line, err := s.readLine(reader, conn)
		if err != nil {
			if errors.Is(err, errCloseConnection) {
				s.writeReply(conn, "421 Limit exceeded, closing connection")
			}
			return
		}
		command, argument := splitCommand(line)
		if command == "DATA" && argument == "" {
			if err := s.handleData(conn, reader, session); err != nil {
				return
			}
			continue
		}
		close, reply := s.handleCommand(command, argument, session)
		if !s.writeReply(conn, reply) {
			return
		}
		if close {
			return
		}
	}
}

func (s *SMTPServer) handleCommand(command, argument string, session *smtpSession) (bool, string) {
	switch command {
	case "HELO", "EHLO":
		if argument == "" || strings.ContainsAny(argument, " \t") {
			return false, "501 Invalid HELO parameter"
		}
		*session = *newSMTPSession()
		session.hello = true
		if command == "EHLO" {
			return false, "250-localhost greets " + argument + eol + "250 HELP"
		}
		return false, "250 localhost greets " + argument
	case "MAIL":
		if !session.hello {
			return false, "503 Send HELO or EHLO first"
		}
		value, ok := cutPrefixFold(argument, "FROM:")
		if !ok {
			return false, "501 Usage: MAIL FROM:<address>"
		}
		address, _, err := parseAddressArgument(strings.TrimSpace(value))
		if err != nil {
			return false, "501 Invalid sender address"
		}
		session.senderSet = true
		session.sender = address
		session.recipients = nil
		session.recipientSet = map[string]struct{}{}
		return false, "250 Sender OK"
	case "RCPT":
		if !session.hello {
			return false, "503 Send HELO or EHLO first"
		}
		if !session.senderSet {
			return false, "503 Send MAIL FROM first"
		}
		value, ok := cutPrefixFold(argument, "TO:")
		if !ok {
			return false, "501 Usage: RCPT TO:<address>"
		}
		address, _, err := parseAddressArgument(strings.TrimSpace(value))
		if err != nil {
			return false, "501 Invalid recipient address"
		}
		if _, ok := s.cfg.AllowedMailbox[address]; !ok {
			return false, "550 Mailbox not allowed"
		}
		if len(session.recipients) >= s.cfg.MaxRecipients {
			return true, "452 Too many recipients"
		}
		if _, exists := session.recipientSet[address]; !exists {
			session.recipientSet[address] = struct{}{}
			session.recipients = append(session.recipients, address)
		}
		return false, "250 Recipient OK"
	case "RSET":
		if argument != "" {
			return false, "501 RSET accepts no parameter"
		}
		hello := session.hello
		*session = *newSMTPSession()
		session.hello = hello
		return false, "250 Reset OK"
	case "NOOP":
		return false, "250 OK"
	case "QUIT":
		return true, "221 Bye"
	default:
		return false, "500 Command not recognized"
	}
}

func (s *SMTPServer) handleData(conn net.Conn, reader *bufio.Reader, session *smtpSession) error {
	if !session.hello {
		s.writeReply(conn, "503 Send HELO or EHLO first")
		return nil
	}
	if !session.senderSet {
		s.writeReply(conn, "503 Send MAIL FROM first")
		return nil
	}
	if len(session.recipients) == 0 {
		s.writeReply(conn, "503 Send at least one RCPT TO first")
		return nil
	}
	if !s.writeReply(conn, "354 Start mail input; end with <CRLF>.<CRLF>") {
		return errCloseConnection
	}

	var raw bytes.Buffer
	for {
		line, err := s.readLine(reader, conn)
		if err != nil {
			if errors.Is(err, errCloseConnection) {
				s.writeReply(conn, "421 Limit exceeded, closing connection")
			}
			return err
		}
		if bytes.Equal(line, []byte("."+eol)) {
			break
		}
		if int64(raw.Len()+len(line)) > s.cfg.MaxMessageBytes {
			s.writeReply(conn, "552 Message size limit exceeded")
			return errCloseConnection
		}
		if line[0] == '.' {
			line = line[1:]
		}
		raw.Write(line)
	}

	id, err := s.store.SaveMessage(session.sender, session.recipients, raw.Bytes())
	if err != nil {
		s.writeReply(conn, "451 Local storage error")
		return errCloseConnection
	}
	hello := session.hello
	*session = *newSMTPSession()
	session.hello = hello
	s.writeReply(conn, "250 OK: queued as "+id)
	return nil
}

func (s *SMTPServer) readLine(reader *bufio.Reader, conn net.Conn) ([]byte, error) {
	conn.SetReadDeadline(time.Now().Add(time.Duration(s.cfg.ReadDeadlineSec) * time.Second))
	line, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return line, errCloseConnection
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return line, err
	}
	if len(line) == 0 || line[len(line)-1] != '\n' || len(line) < 2 || line[len(line)-2] != '\r' || bytes.Count(line, []byte{'\r'}) != 1 {
		return line, io.EOF
	}
	if len(line) > s.cfg.MaxLineBytes {
		return line, errCloseConnection
	}
	return line, nil
}

func (s *SMTPServer) writeReply(conn net.Conn, reply string) bool {
	conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err := io.WriteString(conn, reply+eol)
	return err == nil
}

func splitCommand(line []byte) (string, string) {
	text := strings.TrimSuffix(string(line), eol)
	fields := strings.SplitN(text, " ", 2)
	command := strings.ToUpper(fields[0])
	argument := ""
	if len(fields) == 2 {
		argument = strings.TrimSpace(fields[1])
	}
	return command, argument
}

func cutPrefixFold(value, prefix string) (string, bool) {
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return "", false
	}
	return value[len(prefix):], true
}
