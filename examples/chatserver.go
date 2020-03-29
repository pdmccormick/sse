package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"go.pdmccormick.com/sse"
)

var (
	httpFlag   = flag.String("http", "localhost:8000", "`host:port` to bind to")
	clientFlag = flag.String("client", "", "`host:port` of server to access")
)

type Poster struct {
	bus sse.Broadcaster
}

type ChatMessage struct {
	Timestamp string `json:"timestamp"`
	Text      string `json:"text"`
}

func main() {
	flag.Parse()

	if *clientFlag != "" {
		url := fmt.Sprintf("http://%s/events", *clientFlag)
		err := client(url)
		if err != nil {
			log.Fatal(err)
		}

		os.Exit(0)
	}

	var p Poster

	http.HandleFunc("/", handleRoot)
	http.HandleFunc("/events", p.handleEvents)
	http.HandleFunc("/post", p.handlePost)

	log.Printf("Server up at http://%s", *httpFlag)

	log.Fatal(http.ListenAndServe(*httpFlag, nil))
}

func client(url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	d := sse.NewDecoder(resp.Body)

	for d.More() {
		msg := &ChatMessage{}
		ev := sse.Event{
			Data: msg,
		}

		if err := d.Decode(&ev); err != nil {
			return err
		}

		log.Printf("Event %+v, Message %+v", ev, msg)
	}

	log.Printf("Closing connection")

	return nil
}

func (p *Poster) handlePost(w http.ResponseWriter, r *http.Request) {
	var msg ChatMessage

	d := json.NewDecoder(r.Body)
	err := d.Decode(&msg)
	if err != nil {
		log.Printf("Error decoding: %s", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	log.Printf("Message: %+v", msg.Text)

	msg.Timestamp = time.Now().Format(time.UnixDate)

	ev := sse.Event{
		Data: msg,
	}

	ev.WriteTo(&p.bus)
}

func (p *Poster) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := fmt.Sprintf("Client %s", r.RemoteAddr)

	log.Printf("New connection from %s", id)

	ew, err := sse.EventWriter(w)
	if err != nil {
		panic(err)
	}

	p.bus.Add(ew)
	defer p.bus.Remove(ew)

	defer func() {
		msg := ChatMessage{
			Timestamp: time.Now().Format(time.UnixDate),
			Text:      fmt.Sprintf("Disconnect for %s", id),
		}

		ev := sse.Event{
			Data: msg,
		}

		ev.WriteTo(&p.bus)
	}()

	msg := ChatMessage{
		Timestamp: time.Now().Format(time.UnixDate),
		Text:      fmt.Sprintf("New connection from %s", id),
	}

	ev := sse.Event{
		Data: msg,
	}

	ev.WriteTo(&p.bus)

	select {
	case <-r.Context().Done():
		log.Printf("Connection closed for %s", id)
	}
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "text/html")

	fmt.Fprintf(w, `
<h1>Chat Server</h1>

<form id='entry'>
	<input type='text' id='message'>
</form>

<ul id='container' reversed='reversed'>
	<div></div>
</ul>

<script>
function doPost(url, data) {
	return fetch(url, {
		method: 'POST',
		headers: {
			'Content-Type': 'application/json',
		},
		body: JSON.stringify(data),
	}).then(function (resp) {
		console.log(resp);
	});
}

function formSubmit(ev) {
	ev.preventDefault();

	var input = document.querySelector('#message');

	var payload = {
		text: input.value,
	};

	doPost('/post', payload);

	input.value = '';
}

function logMessage(html) {
	var item = document.createElement('li');
	item.innerHTML = html;

	var container = document.querySelector('#container');
	container.insertBefore(item, container.childNodes[0]);
}

document.addEventListener("DOMContentLoaded", function() {
	var source = new EventSource('/events');

	document.querySelector('#message').focus();

	document.querySelector('#entry').addEventListener('submit', formSubmit);

	source.onopen = function(e) {
		logMessage('Connection opened');
	}

	source.onmessage = function(e) {
		console.debug(e);
		var data = JSON.parse(e.data);

		console.log(data);

		var html = data.timestamp + ': ' + data.text;
		logMessage(html);
	}

	source.addEventListener('update', function(e) {
		console.log('update');
		source.onmessage(e);
	});

	source.onclose = function(e) {
		logMessage('Connection closed');
	}

	source.onerror = function(e) {
		console.log(e);
		logMessage('Connection error');
	};
});
</script>
`)
}
