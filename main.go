package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

//go:embed error.html.gotmpl
var errorTmplStr string
var errorTmpl = template.Must(template.New("error").Parse(errorTmplStr))

var watchChannels = []chan struct{}{}
var watchLock sync.RWMutex
var data = map[string]any{}
var dataLock sync.RWMutex

func main() {
	addr := flag.String("addr", "localhost:9654", "addr")
	templatePath := flag.String("path", "template.gotmpl", "path of file to be rendered")
	dataValue := flag.String("data", "", "data")
	flag.Parse()

	if strings.HasPrefix(*dataValue, "@") {
		content, err := os.ReadFile((*dataValue)[1:])
		if err != nil {
			panic("ERROR: could not read data file: " + err.Error())
		}
		*dataValue = string(content)
	}
	if *dataValue == "" {
		*dataValue = "{}"
	}
	if err := json.Unmarshal([]byte(*dataValue), &data); err != nil {
		panic("ERROR: could not parse data file: " + err.Error())
	}

	if _addr, found := os.LookupEnv("PORT"); found {
		*addr = _addr
	}

	watch(*templatePath)

	mux := http.NewServeMux()
	mux.Handle("/", index(*templatePath))
	mux.Handle("/sse", sse(*templatePath))
	mux.Handle("/data", setData())
	log.Printf("Going to preview %s on http://%s", *templatePath, *addr)
	panic(http.ListenAndServe(*addr, mux))
}

func watch(file string) {
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
					triggerReload()
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

func triggerReload() {
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

func index(templatePath string) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/" {
			rw.WriteHeader(404)
			return
		}
		rw.Write(renderTemplate(templatePath, true))
	})
}

func setData() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.Method != "POST" {
			rw.WriteHeader(405)
			return
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			rw.WriteHeader(500)
			log.Printf("ERROR: could read body: %s", err.Error())
		}
		req.Body.Close()

		dataLock.Lock()
		defer dataLock.Unlock()
		data = map[string]any{}
		if err = json.Unmarshal(body, &data); err != nil {
			rw.WriteHeader(500)
			log.Printf("ERROR: could not parse body: %s", err.Error())
		}
		triggerReload()
	})
}

func sse(templatePath string) http.Handler {
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
			lines := bytes.Split(renderTemplate(templatePath, false), []byte("\n"))
			for _, line := range lines {
				fmt.Fprintf(w, "data: %s\n", line)
			}
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

func renderTemplate(templatePath string, doPatch bool) []byte {
	buf := bytes.NewBufferString("")
	dir := path.Dir(templatePath)
	tmpl, err := template.ParseGlob(dir + "/*")
	if err != nil {
		errorTmpl.Execute(buf, err)
		return buf.Bytes()
	}
	dataLock.RLock()
	defer dataLock.RUnlock()
	if err := tmpl.ExecuteTemplate(buf, path.Base(templatePath), data); err != nil {
		errorTmpl.Execute(buf, err)
		return buf.Bytes()
	}
	if doPatch {
		str, _ := json.MarshalIndent(data, "", "    ")
		return patch(buf.Bytes(), str)
	} else {
		return buf.Bytes()
	}
}

func script(content []byte) []byte {
	return []byte(fmt.Sprintf(`
	<script id="sse-script" type="text/javascript">
		let editorData = `+"`%s`"+`;
		let valid = true;
		window.addEventListener('load', () => {
			const sse = new EventSource("/sse");
			sse.addEventListener("message", ({data}) => {
				const pos = document.getElementById("editor").selectionStart;
				const open = document.getElementById("editor").style.display != "none";
				document.querySelector("html").innerHTML = data;
				addEditor(editorData);
				document.getElementById("editor").selectionStart = pos;
				if(open) {
					document.getElementById("editor").style.display = "block";
					document.getElementById("editor").focus();
				}
				document.dispatchEvent(new Event("DOMContentLoaded"))
			});
			document.getElementById("sse-script").remove();
			addEditor(editorData)
			document.addEventListener('keydown', function(e) {
				if(e.metaKey && e.key === "e") {
					const editor = document.getElementById("editor");
					if(editor.style.display === "none") {
						editor.style.display = "block";
						window.location.hash = "editor";
					}
					else {
						editor.style.display = "none";
						window.location.hash = "";
					}
				}
			})
		});

		function addEditor(content) {
			const editor = document.createElement("textarea")
			editor.style = "display: none; padding: 0.5em; position: fixed; bottom: 0; top: 0; right: 0; width: 33%%; background: rgb(50,50,50); color: white; font-size: 0.8em; outline: none;";
			if(window.location.hash == "#editor") {
				editor.style.display = "block";
			}
			editor.append(document.createTextNode(content));
			editor.id = "editor";
			editor.addEventListener("input", async function(e) {
				editorData = editor.value;

				try {
					const obj = JSON.parse(editorData)
					if(typeof obj != "object") throw "not an object"
					editor.style.background = "rgba(50,50,50)"
					valid = true;
					await fetch("/data", {
						method: "POST",
						body: editorData
					})
				} catch(e) {
					valid = false;
					editor.style.background = "rgba(100,50,50)"
				}
			});
			editor.addEventListener("keydown", async function(e) {
				if(valid && (e.key === "s" || e.key === "Enter") && e.metaKey) {
					e.preventDefault();
					e.stopPropagation();
					await fetch("/data", {
						method: "POST",
						body: editorData
					})
				}
				if(valid && e.key === "f" && e.metaKey && e.shiftKey) {
					e.target.value = JSON.stringify(JSON.parse(editorData), null, "    ")
					editorData = e.target.value
				}
			});
			document.body.appendChild(editor);
		};

	</script>
	`, content))
}

func patch(html []byte, data []byte) []byte {
	pos := bytes.Index(html, []byte("</html>"))
	if pos < 0 {
		return html
	}
	return append(html[:pos], append(script(data), []byte("</html>")...)...)
}
