package main

import (
	//"crypto/md5"
	"encoding/json"
	"errors"
	"flag"

	"math/rand"
	"net/http"
	"net/url"

	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"

	glog "github.com/golang/glog"
)

var upgrader = websocket.Upgrader{CheckOrigin: checkSameOrigin}
var upgraderMutex sync.Mutex

func init() {
	upgrader.Subprotocols = []string{"janus-protocol"}
}

func checkSameOrigin(r *http.Request) bool {
	origin := r.Header["Origin"]
	if len(origin) == 0 {
		return true
	}
	u, err := url.Parse(origin[0])
	if err != nil {
		return false
	}
	//fmt.Println("u.Host",u.Host)
	//fmt.Println("r.Host",r.Host)
	//return equalASCIIFold(u.Host, r.Host)
	_ = u
	return true
}

func equalASCIIFold(s, t string) bool {
	for s != "" && t != "" {
		sr, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		tr, size := utf8.DecodeRuneInString(t)
		t = t[size:]
		if sr == tr {
			continue
		}
		if 'A' <= sr && sr <= 'Z' {
			sr = sr + 'a' - 'A'
		}
		if 'A' <= tr && tr <= 'Z' {
			tr = tr + 'a' - 'A'
		}
		if sr != tr {
			return false
		}
	}
	return s == t
}

func check(err error) {
	if err != nil {
		glog.Fatal(err)
		panic(err)
	}
}

// from clients or servers to messageHandler
type message struct {
	channel    string
	mtype      int
	message    []byte
	fromServer bool
	conn       *websocket.Conn
}

var messages = make(chan message)

type key struct {
	channel    string
	fromServer bool
}
type connection struct {
	myconn *websocket.Conn
	paired *websocket.Conn
}

var connections = make(map[key]connection)

// Transaction holds details from transactions tags found in json messages
type Transaction struct {
	clientConn *websocket.Conn
	received   time.Time
	iscreate   bool
}

var transactions = make(map[string]Transaction)

var janusSessions = make(map[uint64]Transaction)

// JanusJSON is used to pull transaction ids from json
type JanusJSON struct {
	Janus         string `json:"janus"`
	TransactionID string `json:"transaction"`
	SessionID     uint64 `json:"session_id"`

	Data struct {
		ID uint64 `json:"id"`
	} `json:"data"`
}

//this doesnt touch the state for simplicty's sake
func wsHandler(w http.ResponseWriter, r *http.Request) {
	params := mux.Vars(r)
	channel := params["channel"]
	clientServer := params["clientServer"]

	var fromServer bool

	switch clientServer {
	case "client":
		fromServer = false
	case "server":
		fromServer = true
	default:
		http.Error(w, "bad path not /x/client or /x/server", http.StatusNotFound)
		return

	}

	upgraderMutex.Lock()
	c, err := upgrader.Upgrade(w, r, nil)
	upgraderMutex.Unlock()
	check(err)
	defer func() {
		check(c.Close())
	}()

	connections[key{channel, fromServer}] = connection{myconn: c, paired: nil}

	glog.Warningln("connection from ", c.RemoteAddr())

	for {
		mtype, msg, err := c.ReadMessage()
		if websocket.IsCloseError(err) {
			glog.Warningln("remote conn closed, dropping off")
			return
		}
		check(err)

		messages <- message{channel, mtype, msg, fromServer, c}
	}
}

//all data struct manipulation happens on one place, no mutexes
func messageHandler() {
	var err error

	cleanupTicker := time.NewTicker(time.Minute)

	for {
		select {
		case _ = <-cleanupTicker.C:
			for txid, txdetails := range transactions {
				if time.Since(txdetails.received) > 10*time.Minute {
					delete(transactions, txid)
				}
			}

		case m := <-messages:

			// find this connetion
			mykey := key{m.channel, m.fromServer}
			x, found := connections[mykey]
			if !found {
				panic(errors.New("not found xyx"))
			}

			// find transaction number in json
			var t JanusJSON
			err = json.Unmarshal(m.message, &t)
			// if err != nil || t.TransactionID == "" {
			// 	log.Tracef("no transaction field. discard. fromserver: %v %s\n", m.fromServer, m.message)
			// 	break
			// }
			glog.V(2).Infoln("idxxx", t.Data.ID)

			if m.fromServer {
				clientConn := (*websocket.Conn)(nil)

				if len(t.TransactionID) > 0 {
					txc, found := transactions[t.TransactionID]
					if !found {
						glog.Warningln("no transaction id. discard.")
						//continue
					} else {
						clientConn = txc.clientConn
						if t.Data.ID > 0 && txc.iscreate {
							glog.V(2).Infoln("idxxx found new session_id", t.Data.ID)
							janusSessions[t.Data.ID] = Transaction{clientConn: txc.clientConn, received: time.Now()}
						}
					}
				} else {
					if t.SessionID > 0 {
						sessioninfo, found := janusSessions[t.SessionID]
						if found {
							clientConn = sessioninfo.clientConn
							glog.V(2).Infoln("no transaction id. but found session, day saved!.")
						}
					}
				}
				if clientConn == nil {
					glog.Fatal("dont know what do with server message")
				}

				// testing one to one mode, without transacrion matching				// for k, v := range connections {
				// 	if k.fromServer == false {
				// 		clientConn = v.myconn
				// 	}
				// }
				if clientConn == nil {
					continue
				}

				// forward message to paired client

				//err = txc.clientConn.WriteMessage(m.mtype, m.message)
				err = clientConn.WriteMessage(websocket.TextMessage, m.message)
				if websocket.IsCloseError(err) {
					glog.V(1).Infof("write to client %s on closed connection", clientConn.RemoteAddr())
				}
				glog.V(2).Infoln("to client", string(m.message))

				glog.V(2).Infof("forwarded 1 s->c rem:%v -> rem:%v",
					m.conn.RemoteAddr(),
					clientConn.RemoteAddr())

			} else {
				// save transaction
				iscreate := t.Janus == "create"
				transactions[t.TransactionID] = Transaction{clientConn: m.conn, received: time.Now(), iscreate: iscreate}

				// does this client have an associated server?
				// if not, associate it
				if x.paired == nil {
					z := make([]connection, 0, len(connections))
					for k, v := range connections {
						if k.fromServer && k.channel == m.channel {
							z = append(z, v)
						}
					}
					randomIndex := rand.Intn(len(z))
					x = connection{myconn: x.myconn, paired: z[randomIndex].myconn}
					connections[mykey] = x
					glog.Warningf("bound client rem:%v to server rem:%v",
						x.myconn.RemoteAddr(),
						x.paired.RemoteAddr())
				}

				glog.V(2).Infoln("to server", string(m.message))
				// forward message to paired server

				err = x.paired.WriteMessage(m.mtype, m.message)
				if websocket.IsCloseError(err) {
					glog.Warningf("write to server %s on closed connection\n", x.paired.RemoteAddr())
				}

				glog.V(2).Infof("forwarded 1 c->s rem:%v -> rem:%v",
					x.myconn.RemoteAddr(),
					x.paired.RemoteAddr())
			}
		}
	}
}

func flusher() {
	for range time.NewTicker(500 * time.Millisecond).C {
		glog.Flush()
	}
}

func main() {
	flag.Parse() //must be first for glog

	defer glog.Flush()
	go flusher()

	// in order:   INFO WARNING ERROR FATAL
	// https://raw.githubusercontent.com/golang/glog/master/glog.go

	//logging levels
	// 0 nothing
	// 1 info   conecttion status, non fatal errors
	// 2 everything  messages

	// these settings mean:
	//glog.V() only goes to file
	//glog.Info()  only goes to file
	//glog.Warn or higher to go both file, stderr
	//
	flag.Lookup("log_dir").Value.Set(".")
	//careful these interact! flag.Lookup("alsologtostderr").Value.Set("true")
	flag.Lookup("stderrthreshold").Value.Set("WARNING")
	flag.Lookup("v").Value.Set("2")

	// glog.Info("stderr and file!")
	glog.V(1).Info("glog-v(1)") // to file only, not stderr
	glog.Info("glog-info")      // to file only, not stderr
	glog.Warning("glog-warn")	// >=WARN to file AND stderr
	glog.Flush()



	go messageHandler()

	r := mux.NewRouter()
	r.HandleFunc("/{clientServer}/{channel}", wsHandler).Methods("GET")
	http.Handle("/", r)
	panic(http.ListenAndServe(":9999", nil))

}
