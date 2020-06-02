package sse // import "go.pdmccormick.com/sse"

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Event struct {
	Id      string
	Event   string
	RawData string
	Data    interface{}
	Retry   time.Duration
	Comment string
}

func (ev *Event) WriteTo(w io.Writer) (n int64, err error) {
	var (
		sep bool
		buf = &bytes.Buffer{}
		je  = json.NewEncoder(buf)
	)

	if ev.Comment != "" {
		for _, line := range strings.Split(ev.Comment, "\n") {
			if _, err := fmt.Fprintf(buf, ":%s\n", line); err != nil {
				return int64(buf.Len()), err
			}
		}
	}

	if ev.Id != "" {
		if _, err := fmt.Fprintf(buf, "id: %s\n", ev.Id); err != nil {
			return int64(buf.Len()), err
		}

		sep = true
	}

	if ev.Event != "" {
		if _, err := fmt.Fprintf(buf, "event: %s\n", ev.Event); err != nil {
			return int64(buf.Len()), err
		}

		sep = true
	}

	if ev.Retry != 0 {
		if _, err := fmt.Fprintf(buf, "retry: %d\n", ev.Retry.Milliseconds()); err != nil {
			return int64(buf.Len()), err
		}

		sep = true
	}

	if ev.RawData != "" {
		for _, line := range strings.Split(ev.RawData, "\n") {
			if _, err := fmt.Fprintf(buf, "data: %s\n", line); err != nil {
				return int64(buf.Len()), err
			}

			sep = true
		}
	} else if ev.Data != nil {
		if _, err := buf.Write([]byte("data: ")); err != nil {
			return int64(buf.Len()), err
		}

		// NB: `Encode` will write a newline character after the JSON encoding of `data`
		if err := je.Encode(ev.Data); err != nil {
			return int64(buf.Len()), err
		}

		sep = true
	}

	if sep {
		if _, err := buf.Write([]byte("\n")); err != nil {
			return int64(buf.Len()), err
		}
	}

	return buf.WriteTo(w)
}

type Decoder struct {
	s       *bufio.Scanner
	b       bytes.Buffer
	j       *json.Decoder
	hasNext bool
	ev      Event
	err     error
}

func NewDecoder(r io.Reader) *Decoder {
	dec := &Decoder{
		s: bufio.NewScanner(r),
	}

	dec.j = json.NewDecoder(&dec.b)

	return dec
}

func (dec *Decoder) More() bool {
	if dec.hasNext && dec.err == nil {
		return true
	}

	if err := dec.next(); err != nil {
		dec.err = err
		return false
	}

	return true
}

func (dec *Decoder) Err() error {
	switch err := dec.err; err {
	case io.EOF, io.ErrUnexpectedEOF:
		return nil

	default:
		return err
	}
}

func (dec *Decoder) next() error {
	dec.b.Reset()
	dec.ev = Event{}

	var (
		once      bool
		comments  []string
		firstData = true
	)

	for dec.s.Scan() {
		once = true
		line := dec.s.Text()

		if line == "" {
			break
		}

		var field, value string

		i := strings.Index(line, ":")

		if i == -1 {
			// No colon, treat entire line as field name with an empty value string
			field = line
			value = ""
		} else if i == 0 {
			comments = append(comments, line[1:])
			continue
		} else {
			field = line[:i]
			value = line[i+1:]

			if value[0] == ' ' {
				value = value[1:]
			}
		}

		switch field {
		case "id":
			dec.ev.Id = value

		case "data":
			if _, err := dec.b.WriteString(value); err != nil {
				return err
			}

			if firstData {
				firstData = false
			} else {
				if err := dec.b.WriteByte('\n'); err != nil {
					return err
				}
			}

		case "event":
			dec.ev.Event = value

		case "retry":
			if retry, err := strconv.ParseUint(value, 10, 64); err != nil {
				continue
			} else {
				dec.ev.Retry = time.Duration(retry) * time.Millisecond
			}
		}
	}

	if err := dec.s.Err(); err != nil {
		return err
	}

	if !once {
		return io.EOF
	}

	if len(comments) > 0 {
		dec.ev.Comment = strings.Join(comments, "\n")
	} else {
		dec.ev.Comment = ""
	}

	dec.hasNext = true

	return nil
}

func (dec *Decoder) Decode(ev *Event) error {
	if !dec.hasNext {
		if err := dec.next(); err != nil {
			return err
		}
	}

	data := ev.Data
	*ev = dec.ev
	ev.Data = data
	ev.RawData = string(dec.b.Bytes())

	dec.hasNext = false

	if dec.b.Len() > 0 && data != nil {
		if err := dec.j.Decode(data); err != nil {
			return err
		}
	}

	return nil
}

type Broadcaster struct {
	mu sync.RWMutex
	ws map[io.Writer]chan<- error
}

func (br *Broadcaster) Add(w io.Writer, errc chan<- error) {
	br.mu.Lock()
	defer br.mu.Unlock()

	if br.ws == nil {
		br.ws = make(map[io.Writer]chan<- error)
	}

	br.ws[w] = errc
}

func (br *Broadcaster) Remove(w io.Writer) {
	br.mu.Lock()
	defer br.mu.Unlock()

	if br.ws == nil {
		return
	}

	delete(br.ws, w)
}

func (br *Broadcaster) Write(data []byte) (int, error) {
	br.mu.RLock()
	defer br.mu.RUnlock()

	if br.ws == nil {
		return 0, nil
	}

	wg := &sync.WaitGroup{}

	for w, errc := range br.ws {
		var (
			w    = w
			errc = errc
		)

		wg.Add(1)

		go func() {
			defer wg.Done()

			if _, err := w.Write(data); err != nil {
				br.mu.Lock()
				defer br.mu.Unlock()

				delete(br.ws, w)

				if errc != nil {
					errc <- err
					close(errc)
				}
			}
		}()
	}

	wg.Wait()

	return len(data), nil
}

const (
	ContentType = "text/event-stream"
)

func EventWriter(w http.ResponseWriter) (io.Writer, error) {
	f, ok := w.(http.Flusher)
	if !ok {
		err := fmt.Errorf("%T does not implement http.Flusher", w)
		return nil, err
	}

	w.Header().Set("Content-Type", ContentType)
	f.Flush()

	var fw = flushWriter{w: w, f: f}

	return &fw, nil
}

type flushWriter struct {
	mu sync.Mutex
	w  io.Writer
	f  http.Flusher
}

func (fw *flushWriter) Write(data []byte) (int, error) {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	n, err := fw.w.Write(data)
	fw.f.Flush()

	return n, err
}
