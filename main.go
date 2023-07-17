package main

import (
	"bytes"
	_ "embed"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

//go:embed error.html.gotmpl
var errorTmplStr string
var errorTmpl = template.Must(template.New("error").Parse(errorTmplStr))

var watchChannels = []chan struct{}{}
var watchLock sync.RWMutex

func main() {
	addr := flag.String("addr", "localhost:9654", "addr")
	path := flag.String("path", "template.gotmpl", "path of file to be rendered")
	flag.Parse()

	if _addr, found := os.LookupEnv("PORT"); found {
		*addr = _addr
	}

	watch(*path, watchChannels)

	mux := http.NewServeMux()
	mux.Handle("/", index(*path))
	mux.Handle("/sse", sse(*path))
	log.Printf("Going to preview %s on http://%s", *path, *addr)
	panic(http.ListenAndServe(*addr, mux))
}

func watch(file string, chans []chan struct{}) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		panic(err)
	}

	if err := watcher.Add(path.Dir(file)); err != nil {
		panic(err)
	}
	log.Printf("watching %v", watcher.WatchList())

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op == fsnotify.Write {
					log.Printf("file changed: %s", event.String())
					watchLock.Lock()
					for i := len(watchChannels) - 1; i >= 0; i-- {
						select {
						case watchChannels[i] <- struct{}{}: // noop
						default: // ch dead, remove
							watchChannels = append(watchChannels[:i], watchChannels[i+1:]...)
						}
					}
					watchLock.Unlock()
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("error while watching %s: %s", file, err.Error())
			}
		}
	}()
}

func index(path string) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/" {
			rw.WriteHeader(404)
			return
		}
		rw.Write(renderTemplate(path, true))
	})
}

func sse(path string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		run := func() {
			lines := bytes.Split(renderTemplate(path, false), []byte("\n"))
			for _, line := range lines {
				fmt.Fprintf(w, "data: %s\n", line)
			}
			// fmt.Fprint(w, "event: render\ndata:  ok\n\n")
			fmt.Fprintln(w)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}

		ch := make(chan struct{})
		watchLock.Lock()
		watchChannels = append(watchChannels, ch)
		watchLock.Unlock()

	loop:
		for {
			select {
			case <-ch:
				run()
			case <-r.Context().Done():
				// client disconnected
				break loop
			}
		}
	})
}

func renderTemplate(path string, doPatch bool) []byte {
	buf := bytes.NewBufferString("")
	data, err := os.ReadFile(path)
	if err != nil {
		errorTmpl.Execute(buf, fmt.Sprintf("404: could not read %s: %s", path, err.Error()))
		return buf.Bytes()
	}
	tmpl, err := template.New("main").Parse(string(data))
	if err != nil {
		errorTmpl.Execute(buf, err)
		return buf.Bytes()
	}
	if err := tmpl.Execute(buf, map[string]any{"time": time.Now().Format(time.RFC3339)}); err != nil {
		errorTmpl.Execute(buf, err)
		return buf.Bytes()
	}
	if doPatch {
		return patch(buf.Bytes())
	} else {
		return buf.Bytes()
	}
}

var script = []byte(`
	<script id="sse-script" type="text/javascript">
		window.addEventListener('load', () => {
			const sse = new EventSource("/sse");
			sse.addEventListener("message", ({data}) => {
				document.close();
				document.write(data);
			});
			document.getElementById("sse-script").remove();
		});
	</script>`)

func patch(html []byte) []byte {
	pos := bytes.Index(html, []byte("</html>"))
	if pos < 0 {
		return html
	}
	return append(html[:pos], append(script, []byte("</html>")...)...)
}
