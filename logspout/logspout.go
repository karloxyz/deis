package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"code.google.com/p/go.net/websocket"
	"github.com/coreos/go-etcd/etcd"
	dtime "github.com/deis/deis/pkg/time"
	"github.com/fsouza/go-dockerclient"
	"github.com/go-martini/martini"
)

var debugMode bool

func debug(v ...interface{}) {
	if debugMode {
		log.Println(v...)
	}
}

func assert(err error, context string) {
	if err != nil {
		log.Fatalf("%s: %v", context, err)
	}
}

func getopt(name, dfault string) string {
	value := os.Getenv(name)
	if value == "" {
		value = dfault
	}
	return value
}

type Colorizer map[string]int

// returns up to 14 color escape codes (then repeats) for each unique key
func (c Colorizer) Get(key string) string {
	i, exists := c[key]
	if !exists {
		c[key] = len(c)
		i = c[key]
	}
	bright := "1;"
	if i%14 > 6 {
		bright = ""
	}
	return "\x1b[" + bright + "3" + strconv.Itoa(7-(i%7)) + "m"
}

func syslogStreamer(target Target, types []string, logstream chan *Log) {
	typestr := "," + strings.Join(types, ",") + ","
	for logline := range logstream {
		if typestr != ",," && !strings.Contains(typestr, logline.Type) {
			continue
		}
		tag, pid := getLogName(logline.Name)
		addr, err := net.ResolveUDPAddr("udp", target.Addr)
		assert(err, "syslog")
		conn, err := net.DialUDP("udp", nil, addr)
		assert(err, "syslog")
		// bump up the packet size for large log lines
		assert(conn.SetWriteBuffer(1048576), "syslog")
		// HACK: Go's syslog package hardcodes the log format, so let's send our own message
		_, err = fmt.Fprintf(conn,
			"%s %s[%s]: %s",
			time.Now().Format(getopt("DATETIME_FORMAT", dtime.DEIS_DATETIME_FORMAT)),
			tag,
			pid,
			logline.Data)
		assert(err, "syslog")
	}
}

// getLogName returns a custom tag and PID for containers that
// match Deis' specific application name format. Otherwise,
// it returns the original name and 1 as the PID.
func getLogName(name string) (string, string) {
	// example regex that should match: go_v2.web.1
	r := regexp.MustCompile(`(^[a-z0-9-]+)_(v[0-9]+)\.([a-z-_]+\.[0-9]+)$`)
	match := r.FindStringSubmatch(name)
	if match == nil {
		return name, "1"
	} else {
		return match[1], match[3]
	}
}

func websocketStreamer(w http.ResponseWriter, req *http.Request, logstream chan *Log, closer chan bool) {
	websocket.Handler(func(conn *websocket.Conn) {
		for logline := range logstream {
			if req.URL.Query().Get("type") != "" && logline.Type != req.URL.Query().Get("type") {
				continue
			}
			_, err := conn.Write(append(marshal(logline), '\n'))
			if err != nil {
				closer <- true
				return
			}
		}
	}).ServeHTTP(w, req)
}

func httpStreamer(w http.ResponseWriter, req *http.Request, logstream chan *Log, multi bool) {
	var colors Colorizer
	var usecolor, usejson bool
	nameWidth := 16
	if req.URL.Query().Get("colors") != "off" {
		colors = make(Colorizer)
		usecolor = true
	}
	if req.Header.Get("Accept") == "application/json" {
		w.Header().Add("Content-Type", "application/json")
		usejson = true
	} else {
		w.Header().Add("Content-Type", "text/plain")
	}
	for logline := range logstream {
		if req.URL.Query().Get("types") != "" && logline.Type != req.URL.Query().Get("types") {
			continue
		}
		if usejson {
			w.Write(append(marshal(logline), '\n'))
		} else {
			if multi {
				if len(logline.Name) > nameWidth {
					nameWidth = len(logline.Name)
				}
				if usecolor {
					w.Write([]byte(fmt.Sprintf(
						"%s%"+strconv.Itoa(nameWidth)+"s|%s\x1b[0m\n",
						colors.Get(logline.Name), logline.Name, logline.Data,
					)))
				} else {
					w.Write([]byte(fmt.Sprintf(
						"%"+strconv.Itoa(nameWidth)+"s|%s\n", logline.Name, logline.Data,
					)))
				}
			} else {
				w.Write(append([]byte(logline.Data), '\n'))
			}
		}
		w.(http.Flusher).Flush()
	}
}

func main() {
	debugMode = getopt("DEBUG", "") != ""
	port := getopt("PORT", "8000")
	endpoint := getopt("DOCKER_HOST", "unix:///var/run/docker.sock")
	routespath := getopt("ROUTESPATH", "/var/lib/logspout")

	client, err := docker.NewClient(endpoint)
	assert(err, "docker")
	attacher := NewAttachManager(client)
	router := NewRouteManager(attacher)

	// HACK: if we are connecting to etcd, get the logger's connection
	// details from there
	if etcdHost := os.Getenv("ETCD_HOST"); etcdHost != "" {
		connectionString := []string{"http://" + etcdHost + ":4001"}
		debug("etcd:", connectionString[0])
		etcd := etcd.NewClient(connectionString)
		etcd.SetDialTimeout(3 * time.Second)
		hostResp, err := etcd.Get("/deis/logs/host", false, false)
		assert(err, "url")
		portResp, err := etcd.Get("/deis/logs/port", false, false)
		assert(err, "url")
		host := fmt.Sprintf("%s:%s", hostResp.Node.Value, portResp.Node.Value)
		log.Println("routing all to " + host)
		router.Add(&Route{Target: Target{Type: "syslog", Addr: host}})
	}

	if len(os.Args) > 1 {
		u, err := url.Parse(os.Args[1])
		assert(err, "url")
		log.Println("routing all to " + os.Args[1])
		router.Add(&Route{Target: Target{Type: u.Scheme, Addr: u.Host}})
	}

	if _, err := os.Stat(routespath); err == nil {
		log.Println("loading and persisting routes in " + routespath)
		assert(router.Load(RouteFileStore(routespath)), "persistor")
	}

	m := martini.Classic()

	m.Get("/logs(?:/(?P<predicate>[a-zA-Z]+):(?P<value>.+))?", func(w http.ResponseWriter, req *http.Request, params martini.Params) {
		source := new(Source)
		switch {
		case params["predicate"] == "id" && params["value"] != "":
			source.ID = params["value"][:12]
		case params["predicate"] == "name" && params["value"] != "":
			source.Name = params["value"]
		case params["predicate"] == "filter" && params["value"] != "":
			source.Filter = params["value"]
		}

		if source.ID != "" && attacher.Get(source.ID) == nil {
			http.NotFound(w, req)
			return
		}

		logstream := make(chan *Log)
		defer close(logstream)

		var closer <-chan bool
		if req.Header.Get("Upgrade") == "websocket" {
			closerBi := make(chan bool)
			go websocketStreamer(w, req, logstream, closerBi)
			closer = closerBi
		} else {
			go httpStreamer(w, req, logstream, source.All() || source.Filter != "")
			closer = w.(http.CloseNotifier).CloseNotify()
		}

		attacher.Listen(source, logstream, closer)
	})

	m.Get("/routes", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Add("Content-Type", "application/json")
		routes, _ := router.GetAll()
		w.Write(append(marshal(routes), '\n'))
	})

	m.Post("/routes", func(w http.ResponseWriter, req *http.Request) (int, string) {
		route := new(Route)
		if err := unmarshal(req.Body, route); err != nil {
			return http.StatusBadRequest, "Bad request: " + err.Error()
		}

		// TODO: validate?
		router.Add(route)

		w.Header().Add("Content-Type", "application/json")
		return http.StatusCreated, string(append(marshal(route), '\n'))
	})

	m.Get("/routes/:id", func(w http.ResponseWriter, req *http.Request, params martini.Params) {
		route, _ := router.Get(params["id"])
		if route == nil {
			http.NotFound(w, req)
			return
		}
		w.Write(append(marshal(route), '\n'))
	})

	m.Delete("/routes/:id", func(w http.ResponseWriter, req *http.Request, params martini.Params) {
		if ok := router.Remove(params["id"]); !ok {
			http.NotFound(w, req)
		}
	})

	log.Println("logspout serving http on :" + port)
	log.Fatal(http.ListenAndServe(":"+port, m))
}
