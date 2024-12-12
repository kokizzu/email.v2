package email

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"math"
	"math/big"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const (
	MaxLineLength      = 76                             // MaxLineLength is the maximum line length per RFC 2045
	defaultContentType = "text/plain; charset=us-ascii" // defaultContentType is the default Content-Type according to RFC 2045, section 5.2
)

// Attachment is a struct representing an email attachment.
// Based on the mime/multipart.FileHeader struct, Attachment contains the name, MIMEHeader, and content of the attachment in question.
type Attachment struct {
	Filename string
	Header   textproto.MIMEHeader
	Content  []byte
}

type Email struct {
	ReplyTo     []string
	From        string
	To          []string
	Bcc         []string
	Cc          []string
	Subject     string
	Text        []byte // Plaintext message (optional)
	HTML        []byte // Html message (optional)
	Sender      string // override From as SMTP envelope sender (optional)
	Headers     textproto.MIMEHeader
	Attachments []*Attachment
	ReadReceipt []string
}

// Create and initialize an new message struct.
func NewEmail() *Email {
	return &Email{Headers: textproto.MIMEHeader{}}
}

// A custom io.Reader that will trim any leading
// whitespace, as this can cause email imports to fail.
type trimReader struct {
	rd io.Reader
}

// Trims off any unicode whitespace from the originating reader.
func (tr trimReader) Read(buf []byte) (int, error) {
	n, err := tr.rd.Read(buf)
	t := bytes.TrimLeftFunc(buf[:n], unicode.IsSpace)
	n = copy(buf, t)
	return n, err
}

// Reads a stream of bytes from an io.Reader, and returns an email struct containing the parsed data.
// This function expects the data in RFC 5322 format.
func NewEmailFromReader(r io.Reader) (*Email, error) {
	e := NewEmail()
	s := trimReader{rd: r}
	tp := textproto.NewReader(bufio.NewReader(s))
	// Parse the main headers
	hdrs, err := tp.ReadMIMEHeader()
	if err != nil {
		return e, err
	}
	// Set the subject, to, cc, bcc, and from
	for h, v := range hdrs {
		switch h {
		case "Subject":
			e.Subject = v[0]
			subj, err := (&mime.WordDecoder{}).DecodeHeader(e.Subject)
			if err == nil && len(subj) > 0 {
				e.Subject = subj
			}
			delete(hdrs, h)
		case "To":
			for _, to := range v {
				tt, err := (&mime.WordDecoder{}).DecodeHeader(to)
				if err == nil {
					e.To = append(e.To, tt)
				} else {
					e.To = append(e.To, to)
				}
			}
			delete(hdrs, h)
		case "Cc":
			for _, cc := range v {
				tcc, err := (&mime.WordDecoder{}).DecodeHeader(cc)
				if err == nil {
					e.Cc = append(e.Cc, tcc)
				} else {
					e.Cc = append(e.Cc, cc)
				}
			}
			delete(hdrs, h)
		case "Bcc":
			for _, bcc := range v {
				tbcc, err := (&mime.WordDecoder{}).DecodeHeader(bcc)
				if err == nil {
					e.Bcc = append(e.Bcc, tbcc)
				} else {
					e.Bcc = append(e.Bcc, bcc)
				}
			}
			delete(hdrs, h)
		case "From":
			e.From = v[0]
			fr, err := (&mime.WordDecoder{}).DecodeHeader(e.From)
			if err == nil && len(fr) > 0 {
				e.From = fr
			}
			delete(hdrs, h)
		}
	}
	e.Headers = hdrs
	body := tp.R
	// Recursively parse the MIME parts
	ps, err := ParseMIMEParts(body, e.Headers)
	if err != nil {
		return e, err
	}
	for _, p := range ps {
		if ct := p.Header.Get("Content-Type"); ct == "" {
			return e, ErrMissingContentType
		}
		ct, _, err := mime.ParseMediaType(p.Header.Get("Content-Type"))
		if err != nil {
			return e, err
		}
		switch {
		case ct == "text/plain":
			e.Text = p.Body.Bytes()
		case ct == "text/html":
			e.HTML = p.Body.Bytes()
		}
	}
	return e, nil
}

// Part is a copyable representation of a multipart.Part
type Part struct {
	Header textproto.MIMEHeader
	Body   bytes.Buffer
}

func decodePart(iRdr io.Reader, mHdr textproto.MIMEHeader) (Part, error) {
	if mHdr.Get("Content-Transfer-Encoding") == "base64" {
		iRdr = base64.NewDecoder(base64.StdEncoding, iRdr)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, iRdr); err != nil {
		return Part{}, err
	}
	return Part{Body: buf, Header: mHdr}, nil
}

/*
parseMIMEParts will recursively walk a MIME entity and return a []mime.Part
containing each (flattened) mime.Part found. It is important to note that there
are no limits to the number of recursions, so be careful when parsing unknown
MIME structures!
*/
func ParseMIMEParts(iRdr io.Reader, mHdr textproto.MIMEHeader) ([]Part, error) {

	// If no content type is given, set it to the default
	if _, ok := mHdr["Content-Type"]; !ok {
		mHdr.Set("Content-Type", defaultContentType)
	}

	ct, params, err := mime.ParseMediaType(mHdr.Get("Content-Type"))
	if err != nil {
		return nil, err
	}

	// If it's a multipart email, recursively parse the parts
	var ps []Part
	if strings.HasPrefix(ct, "multipart/") {

		if _, ok := params["boundary"]; !ok {
			return ps, ErrMissingBoundary
		}
		mr := multipart.NewReader(iRdr, params["boundary"])

		for {
			pPart, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return ps, err
			}
			if _, ok := pPart.Header["Content-Type"]; !ok {
				pPart.Header.Set("Content-Type", defaultContentType)
			}
			subct, _, err := mime.ParseMediaType(pPart.Header.Get("Content-Type"))
			if err != nil {
				return ps, err
			}

			if strings.HasPrefix(subct, "multipart/") {

				sps, err := ParseMIMEParts(pPart, pPart.Header)
				if err != nil {
					return ps, err
				}
				ps = append(ps, sps...)

			} else {

				part, eP := decodePart(pPart, pPart.Header)
				if eP != nil {
					return ps, eP
				}
				ps = append(ps, part)
			}
		}

	} else {

		part, eP := decodePart(iRdr, mHdr)
		if eP != nil {
			return ps, eP
		}
		ps = append(ps, part)
	}

	return ps, nil
}

// Attaches content from an io.Reader to the email.
// The function will return the created Attachment for reference.
func (e *Email) Attach(r io.Reader, filename string, contentType string) (*Attachment, error) {
	var buffer bytes.Buffer
	if _, err := io.Copy(&buffer, r); err != nil {
		return nil, err
	}
	at := &Attachment{
		Filename: filename,
		Header:   textproto.MIMEHeader{},
		Content:  buffer.Bytes(),
	}
	if contentType != "" {
		at.Header.Set("Content-Type", contentType)
	} else {
		at.Header.Set("Content-Type", "application/octet-stream")
	}
	at.Header.Set("Content-Disposition", fmt.Sprintf("attachment;\r\n filename=\"%s\"", filename))
	at.Header.Set("Content-ID", fmt.Sprintf("<%s>", filename))
	at.Header.Set("Content-Transfer-Encoding", "base64")
	e.Attachments = append(e.Attachments, at)
	return at, nil
}

// Attaches content to the email via filesystem.
// It attempts to open the file referenced by filename and, if successful, creates an Attachment.
// This Attachment is then appended to the slice of Email.Attachments.
// The function will then return the Attachment for reference.
func (e *Email) AttachFile(filename string) (*Attachment, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	ct := mime.TypeByExtension(filepath.Ext(filename))
	basename := filepath.Base(filename)
	return e.Attach(f, basename, ct)
}

// msgHeaders merges the Email's various fields and custom headers together in a
// standards compliant way to create a MIMEHeader to be used in the resulting
// message. It does not alter e.Headers.
//
// "e"'s fields To, Cc, From, Subject will be used unless they are present in
// e.Headers. Unless set in e.Headers, "Date" will filled with the current time.
func (e *Email) msgHeaders() (textproto.MIMEHeader, error) {
	res := make(textproto.MIMEHeader, len(e.Headers)+6)
	if e.Headers != nil {
		for _, h := range []string{"Reply-To", "To", "Cc", "From", "Subject", "Date", "Message-Id", "MIME-Version"} {
			if v, ok := e.Headers[h]; ok {
				res[h] = v
			}
		}
	}
	// Set headers if there are values.
	if _, ok := res["Reply-To"]; !ok && len(e.ReplyTo) > 0 {
		res.Set("Reply-To", strings.Join(e.ReplyTo, ", "))
	}
	if _, ok := res["To"]; !ok && len(e.To) > 0 {
		res.Set("To", strings.Join(e.To, ", "))
	}
	if _, ok := res["Cc"]; !ok && len(e.Cc) > 0 {
		res.Set("Cc", strings.Join(e.Cc, ", "))
	}
	if _, ok := res["Subject"]; !ok && e.Subject != "" {
		res.Set("Subject", e.Subject)
	}
	if _, ok := res["Message-Id"]; !ok {
		id, err := generateMessageID()
		if err != nil {
			return nil, err
		}
		res.Set("Message-Id", id)
	}
	// Date and From are required headers.
	if _, ok := res["From"]; !ok {
		res.Set("From", e.From)
	}
	if _, ok := res["Date"]; !ok {
		res.Set("Date", time.Now().Format(time.RFC1123Z))
	}
	if _, ok := res["MIME-Version"]; !ok {
		res.Set("MIME-Version", "1.0")
	}
	for field, vals := range e.Headers {
		if _, ok := res[field]; !ok {
			res[field] = vals
		}
	}
	return res, nil
}

func writeMessage(buff io.Writer, msg []byte, multipart bool, mediaType string, w *multipart.Writer) error {
	if multipart {
		header := textproto.MIMEHeader{
			"Content-Type":              {mediaType + "; charset=UTF-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		}
		if _, err := w.CreatePart(header); err != nil {
			return err
		}
	}

	qp := quotedprintable.NewWriter(buff)
	// Write the text
	if _, err := qp.Write(msg); err != nil {
		return err
	}
	return qp.Close()
}

// Converts the Email object to a []byte representation--including all needed MIMEHeaders, boundaries, etc.
func (e *Email) Bytes() ([]byte, error) {
	// TODO: better guess buffer size
	buff := bytes.NewBuffer(make([]byte, 0, 4096))

	headers, err := e.msgHeaders()
	if err != nil {
		return nil, err
	}

	var (
		isMixed       = len(e.Attachments) > 0
		isAlternative = len(e.Text) > 0 && len(e.HTML) > 0
	)

	var w *multipart.Writer
	if isMixed || isAlternative {
		w = multipart.NewWriter(buff)
	}
	switch {
	case isMixed:
		headers.Set("Content-Type", "multipart/mixed;\r\n boundary="+w.Boundary())
	case isAlternative:
		headers.Set("Content-Type", "multipart/alternative;\r\n boundary="+w.Boundary())
	case len(e.HTML) > 0:
		headers.Set("Content-Type", "text/html; charset=UTF-8")
		headers.Set("Content-Transfer-Encoding", "quoted-printable")
	default:
		headers.Set("Content-Type", "text/plain; charset=UTF-8")
		headers.Set("Content-Transfer-Encoding", "quoted-printable")
	}
	headerToBytes(buff, headers)
	_, err = io.WriteString(buff, "\r\n")
	if err != nil {
		return nil, err
	}

	// Check to see if there is a Text or HTML field
	if len(e.Text) > 0 || len(e.HTML) > 0 {
		var subWriter *multipart.Writer

		if isMixed && isAlternative {
			// Create the multipart alternative part
			subWriter = multipart.NewWriter(buff)
			header := textproto.MIMEHeader{
				"Content-Type": {"multipart/alternative;\r\n boundary=" + subWriter.Boundary()},
			}
			if _, err := w.CreatePart(header); err != nil {
				return nil, err
			}
		} else {
			subWriter = w
		}
		// Create the body sections
		if len(e.Text) > 0 {
			// Write the text
			if err := writeMessage(buff, e.Text, isMixed || isAlternative, "text/plain", subWriter); err != nil {
				return nil, err
			}
		}
		if len(e.HTML) > 0 {
			// Write the HTML
			if err := writeMessage(buff, e.HTML, isMixed || isAlternative, "text/html", subWriter); err != nil {
				return nil, err
			}
		}
		if isMixed && isAlternative {
			if err := subWriter.Close(); err != nil {
				return nil, err
			}
		}
	}
	// Create attachment part, if necessary
	for _, a := range e.Attachments {
		ap, err := w.CreatePart(a.Header)
		if err != nil {
			return nil, err
		}
		// Write the base64Wrapped content to the part
		base64Wrap(ap, a.Content)
	}
	if isMixed || isAlternative {
		if err := w.Close(); err != nil {
			return nil, err
		}
	}
	return buff.Bytes(), nil
}

// base64Wrap encodes the attachment content, and wraps it according to RFC 2045 standards (every 76 chars)
// The output is then written to the specified io.Writer
func base64Wrap(w io.Writer, b []byte) {
	// 57 raw bytes per 76-byte base64 line.
	const maxRaw = 57
	// Buffer for each line, including trailing CRLF.
	buffer := make([]byte, MaxLineLength+len("\r\n"))
	copy(buffer[MaxLineLength:], "\r\n")
	// Process raw chunks until there's no longer enough to fill a line.
	for len(b) >= maxRaw {
		base64.StdEncoding.Encode(buffer, b[:maxRaw])
		w.Write(buffer)
		b = b[maxRaw:]
	}
	// Handle the last chunk of bytes.
	if len(b) > 0 {
		out := buffer[:base64.StdEncoding.EncodedLen(len(b))]
		base64.StdEncoding.Encode(out, b)
		out = append(out, "\r\n"...)
		w.Write(out)
	}
}

// headerToBytes renders "header" to "buff". If there are multiple values for a
// field, multiple "Field: value\r\n" lines will be emitted.
func headerToBytes(buff io.Writer, header textproto.MIMEHeader) {
	for field, vals := range header {
		for _, subval := range vals {
			// bytes.Buffer.Write() never returns an error.
			io.WriteString(buff, field)
			io.WriteString(buff, ": ")
			// Write the encoded header if needed
			switch {
			case field == "Content-Type" || field == "Content-Disposition":
				buff.Write([]byte(subval))
			default:
				buff.Write([]byte(mime.QEncoding.Encode("UTF-8", subval)))
			}
			io.WriteString(buff, "\r\n")
		}
	}
}

var maxBigInt = big.NewInt(math.MaxInt64)

// generateMessageID generates and returns a string suitable for an RFC 2822
// compliant Message-ID, e.g.:
// <1444789264909237300.3464.1819418242800517193@DESKTOP01>
//
// The following parameters are used to generate a Message-ID:
// - The nanoseconds since Epoch
// - The calling PID
// - A cryptographically random int64
// - The sending hostname
func generateMessageID() (string, error) {
	t := time.Now().UnixNano()
	pid := os.Getpid()
	rint, err := rand.Int(rand.Reader, maxBigInt)
	if err != nil {
		return "", err
	}
	h, err := os.Hostname()
	// If we can't get the hostname, we'll use localhost
	if err != nil {
		h = "localhost.localdomain"
	}
	msgid := fmt.Sprintf("<%d.%d.%d@%s>", t, pid, rint, h)
	return msgid, nil
}

// Select and parse an SMTP envelope sender address.
// Choose Email.Sender if set, or fallback to Email.From.
func (MSG *Email) ParseSender() (mail.Address, error) {
	addrParse := MSG.From
	if len(MSG.Sender) > 0 {
		addrParse = MSG.Sender
	}
	addr, err := mail.ParseAddress(addrParse)
	if err != nil {
		return mail.Address{}, err
	}
	return *addr, nil
}

// Returns slice of To, Cc, and Bcc fields, each parsed with mail.ParseAddress().
func (e *Email) ParseToFromAddrs() ([]*mail.Address, error) {

	// Check to make sure there is at least one "from" address
	if e.From == "" {
		return nil, ErrMissingToOrFrom
	}

	// Merge the To, Cc, and Bcc fields
	sAddr := make([]*mail.Address, 0, len(e.To)+len(e.Cc)+len(e.Bcc))

	for _, addr_list := range [][]string{e.To, e.Cc, e.Bcc} {
		for _, addr_txt := range addr_list {

			A, E := mail.ParseAddress(addr_txt)
			if E != nil {
				return nil, E
			}
			sAddr = append(sAddr, A)
		}
	}

	// Check to make sure there is at least one recipient
	if len(sAddr) == 0 {
		return nil, ErrMissingToOrFrom
	}

	return sAddr, nil
}
