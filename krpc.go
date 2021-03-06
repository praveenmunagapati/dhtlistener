package dhtlistener

import (
	"errors"
	"math"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	pingType         = "ping"
	findNodeType     = "find_node"
	getPeersType     = "get_peers"
	announcePeerType = "announce_peer"
)

const (
	genericError  = 201
	serverError   = 202
	protocolError = 203
	unknownError  = 204
)

// packet represents the information receive from udp.
type packet struct {
	data     []byte
	raddr    *net.UDPAddr
	recvTime time.Time
}

// makeQuery returns a query-formed data.
func makeQuery(t, q string, a map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"t": t,
		"y": "q",
		"q": q,
		"a": a,
	}
}

// makeResponse returns a response-formed data.
func makeResponse(t string, r map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"t": t,
		"y": "r",
		"r": r,
	}
}

// makeError returns a err-formed data.
func makeError(t string, errCode int, errMsg string) map[string]interface{} {
	return map[string]interface{}{
		"t": t,
		"y": "e",
		"e": []interface{}{errCode, errMsg},
	}
}

func send(dht *DHT, addr *net.UDPAddr, data map[string]interface{}) error {
	msg, err := Encode(data)
	if err != nil {
		return err
	}
	_, err = dht.conn.WriteToUDP([]byte(msg), addr)
	return err
}

// query represents the query data included queried node and query-formed data.
type query struct {
	tar  *node
	data map[string]interface{}
}

// transaction implements transaction.
type transaction struct {
	*query
	id       string
	response chan struct{}
}

type transactionManager struct {
	*sync.RWMutex
	transactions *syncMap // transid : transaction
	index        *syncMap // query type + addr : transaction
	curTransId   uint64   // MaxInt32
	queryChan    chan *query
	dht          *DHT
}

func newTransactionManager(dht *DHT) *transactionManager {
	return &transactionManager{
		RWMutex:      &sync.RWMutex{},
		transactions: newsyncMap(),
		index:        newsyncMap(),
		queryChan:    make(chan *query, 1024),
		dht:          dht,
	}
}

// genTransID generates a transaction id and returns it.
func (tm *transactionManager) genTransID() string {
	tm.Lock()
	defer tm.Unlock()

	tm.curTransId = (tm.curTransId + 1) % math.MaxUint32
	return string(I64toA(tm.curTransId))
}

func (tm *transactionManager) newTransaction(id string, q *query) *transaction {
	return &transaction{
		id:       id,
		query:    q,
		response: make(chan struct{}, tm.dht.Try+1),
	}
}

// genIndexKey generates an indexed key which consists of queryType and
// address.
func (tm *transactionManager) genIndexKey(queryType, address string) string {
	return strings.Join([]string{queryType, address}, ":")
}

// genIndexKeyByTrans generates an indexed key by a transaction.
func (tm *transactionManager) genIndexKeyByTrans(trans *transaction) string {
	return tm.genIndexKey(trans.data["q"].(string), trans.tar.addr.String())
}

// insert adds a transaction to transactionManager.
func (tm *transactionManager) insert(trans *transaction) {
	tm.Lock()
	defer tm.Unlock()

	tm.transactions.Set(trans.id, trans)
	tm.index.Set(tm.genIndexKeyByTrans(trans), trans)
}

// delete removes a transaction from transactionManager.
func (tm *transactionManager) delete(transID string) {
	v, ok := tm.transactions.Get(transID)
	if !ok {
		return
	}

	tm.Lock()
	defer tm.Unlock()

	trans := v.(*transaction)
	tm.transactions.Delete(trans.id)
	tm.index.Delete(tm.genIndexKeyByTrans(trans))
}

// len returns how many transactions are requesting now.
func (tm *transactionManager) len() int {
	return tm.transactions.Len()
}

// transaction returns a transaction. keyType should be one of 0, 1 which
// represents transId and index each.
func (tm *transactionManager) transaction(key string, keyType int) *transaction {

	sm := tm.transactions
	if keyType == 1 {
		sm = tm.index
	}

	v, ok := sm.Get(key)
	if !ok {
		return nil
	}

	return v.(*transaction)
}

// getByTransID returns a transaction by transID.
func (tm *transactionManager) getByTransID(transID string) *transaction {
	return tm.transaction(transID, 0)
}

// getByIndex returns a transaction by indexed key.
func (tm *transactionManager) getByIndex(index string) *transaction {
	return tm.transaction(index, 1)
}

// transaction gets the proper transaction with whose id is transId and address is addr.
func (tm *transactionManager) filterOne(transID string, addr *net.UDPAddr) *transaction {

	trans := tm.getByTransID(transID)
	if trans == nil || trans.tar.addr.String() != addr.String() {
		return nil
	}

	return trans
}

// query sends the query-formed data to udp and wait for the response.
// When timeout, it will retry `try - 1` times, which means it will query
// `try` times totally.
func (tm *transactionManager) query(q *query, try int) {
	transID := q.data["t"].(string)
	trans := tm.newTransaction(transID, q)

	tm.insert(trans)
	defer tm.delete(trans.id)

	success := false
	for i := 0; i < try; i++ {
		if err := send(tm.dht, q.tar.addr, q.data); err != nil {
			break
		}

		select {
		case <-trans.response:
			success = true
			break
		case <-time.After(time.Second * 15):
		}
	}

	if !success && q.tar.id != nil {
		tm.dht.rt.Remove(q.tar.id)
	}
}

// run starts to listen and consume the query chan.
func (tm *transactionManager) run() {
	var q *query

	for {
		select {
		case q = <-tm.queryChan:
			go tm.query(q, tm.dht.Try)
		}
	}
}

// sendQuery send query-formed data to the chan.
func (tm *transactionManager) sendQuery(no *node, queryType string, a map[string]interface{}) {

	// If the target is self, then stop.
	if (no.id != nil && no.id.RawString() == tm.dht.me.id.RawString()) ||
		tm.getByIndex(tm.genIndexKey(queryType, no.addr.String())) != nil {
		return
	}

	data := makeQuery(tm.genTransID(), queryType, a)
	tm.queryChan <- &query{
		tar:  no,
		data: data,
	}
}

// ping sends ping query to the chan.
func (tm *transactionManager) ping(no *node) {
	tm.sendQuery(no, pingType, map[string]interface{}{
		"id": tm.dht.me.id.RawString(),
	})
}

// findNode sends find_node query to the chan.
func (tm *transactionManager) findNode(no *node, target string) {
	tm.sendQuery(no, findNodeType, map[string]interface{}{
		"id":     tm.dht.me.id.RawString(),
		"target": target,
	})
}

// getPeers sends get_peers query to the chan.
func (tm *transactionManager) getPeers(no *node, infoHash string) {
	tm.sendQuery(no, getPeersType, map[string]interface{}{
		"id":        tm.dht.me.id.RawString(),
		"info_hash": infoHash,
	})
}

// announcePeer sends announce_peer query to the chan.
func (tm *transactionManager) announcePeer(
	no *node, infoHash string, impliedPort, port int, token string) {

	tm.sendQuery(no, announcePeerType, map[string]interface{}{
		"id":           tm.dht.me.id.RawString(),
		"info_hash":    infoHash,
		"implied_port": impliedPort,
		"port":         port,
		"token":        token,
	})
}

// parseKey parses the key in dict data. `t` is type of the keyed value.
// It's one of "int", "string", "map", "list".
func parseKey(data map[string]interface{}, key string, t string) error {
	val, ok := data[key]
	if !ok {
		return errors.New("lack of key")
	}

	switch t {
	case "string":
		_, ok = val.(string)
	case "int":
		_, ok = val.(int)
	case "map":
		_, ok = val.(map[string]interface{})
	case "list":
		_, ok = val.([]interface{})
	default:
		panic("invalid type")
	}

	if !ok {
		return errors.New("invalid key type")
	}

	return nil
}

// parseKeys parses keys. It just wraps parseKey.
func parseKeys(data map[string]interface{}, pairs [][]string) error {
	for _, args := range pairs {
		key, t := args[0], args[1]
		if err := parseKey(data, key, t); err != nil {
			return err
		}
	}
	return nil
}

// parseMessage parses the basic data received from udp.
// It returns a map value.
func parseMessage(data interface{}) (map[string]interface{}, error) {
	response, ok := data.(map[string]interface{})
	if !ok {
		return nil, errors.New("response is not dict")
	}

	if err := parseKeys(
		response, [][]string{{"t", "string"}, {"y", "string"}}); err != nil {
		return nil, err
	}

	return response, nil
}

// handleRequest handles the requests received from udp.
func handleRequest(dht *DHT, addr *net.UDPAddr, response map[string]interface{}) (success bool) {

	t := response["t"].(string)

	if err := parseKeys(response, [][]string{{"q", "string"}, {"a", "map"}}); err != nil {

		send(dht, addr, makeError(t, protocolError, err.Error()))
		return
	}

	q := response["q"].(string)
	a := response["a"].(map[string]interface{})

	if err := parseKey(a, "id", "string"); err != nil {
		send(dht, addr, makeError(t, protocolError, err.Error()))
		return
	}

	id := a["id"].(string)

	if id == dht.me.id.RawString() {
		return
	}

	if len(id) != 20 {
		send(dht, addr, makeError(t, protocolError, "invalid id"))
		return
	}
	/*
		if no := dht.rt.getNode(id); no != nil {
			send(dht, addr, makeError(t, protocolError, "invalid id"))
			return
		}
	*/
	switch q {
	case pingType:
		send(dht, addr, makeResponse(t, map[string]interface{}{
			"id": dht.me.id.RawString(),
		}))
	case findNodeType:
		if false {
			if err := parseKey(a, "target", "string"); err != nil {
				send(dht, addr, makeError(t, protocolError, err.Error()))
				return
			}

			target := a["target"].(string)
			if len(target) != 20 {
				send(dht, addr, makeError(t, protocolError, "invalid target"))
				return
			}

			var nodes string
			targetID := newHashId(target)

			no := dht.rt.getNode(target)
			if no != nil {
				nodes = no.CompactNodeInfo()
			} else {
				nodes = strings.Join(
					dht.rt.GetClosestNodeCompactInfo(targetID, dht.K),
					"",
				)
			}

			send(dht, addr, makeResponse(t, map[string]interface{}{
				"id":    dht.me.id.RawString(),
				"nodes": nodes,
			}))
		}
	case getPeersType:
		if err := parseKey(a, "info_hash", "string"); err != nil {
			send(dht, addr, makeError(t, protocolError, err.Error()))
			return
		}

		infoHash := a["info_hash"].(string)

		if len(infoHash) != 20 {
			send(dht, addr, makeError(t, protocolError, "invalid info_hash"))
			return
		}

		if peers := dht.peers.GetPeers(infoHash, dht.K); len(peers) > 0 {
			// donot reply
		} else {
			targetID := newHashId(infoHash)
			send(dht, addr, makeResponse(t, map[string]interface{}{
				"id":    dht.me.id.RawString(),
				"token": dht.tokens.getToken(addr),
				"nodes": strings.Join(dht.rt.GetClosestNodeCompactInfo(targetID, dht.K), ""),
			}))
		}

		if dht.OnGetPeers != nil {
			dht.OnGetPeers(infoHash, addr.IP.String(), addr.Port)
		}
	case announcePeerType:
		if err := parseKeys(a, [][]string{{"info_hash", "string"}, {"port", "int"},
			{"token", "string"}}); err != nil {

			send(dht, addr, makeError(t, protocolError, err.Error()))
			return
		}

		infoHash := a["info_hash"].(string)
		port := a["port"].(int)
		token := a["token"].(string)

		if !dht.tokens.check(addr, token) {
			return
		}

		if impliedPort, ok := a["implied_port"]; ok &&
			impliedPort.(int) != 0 {

			port = addr.Port
		}

		if false {
			dht.peers.Insert(infoHash, newPeer(addr.IP, port, token))
		}

		if dht.OnAnnouncePeer != nil {
			dht.OnAnnouncePeer(infoHash, addr.IP.String(), port)
		}
	default:
		return
	}

	no, _ := newNode(id, addr.Network(), addr.String())
	dht.rt.Insert(no)
	return true
}

// findOn puts nodes in the response to the routingTable, then if target is in
// the nodes or all nodes are in the routingTable, it stops. Otherwise it
// continues to findNode or getPeers.
func findOn(dht *DHT, r map[string]interface{}, target *hashid, queryType string) error {

	if err := parseKey(r, "nodes", "string"); err != nil {
		return err
	}

	nodes := r["nodes"].(string)
	if len(nodes)%26 != 0 {
		return errors.New("the length of nodes should can be divided by 26")
	}

	hasNew, found := false, false
	for i := 0; i < len(nodes)/26; i++ {
		no, _ := newNodeFromCompactInfo(string(nodes[i*26 : (i+1)*26]))

		if no.id.RawString() == target.RawString() {
			found = true
		}

		if dht.rt.Insert(no) {
			hasNew = true
		}
	}

	if found || !hasNew {
		return nil
	}

	targetID := target.RawString()
	for _, no := range dht.rt.FindClosestNode(target, dht.K) {
		switch queryType {
		case findNodeType:
			dht.transacts.findNode(no, targetID)
		case getPeersType:
			dht.transacts.getPeers(no, targetID)
		default:
			panic("invalid find type")
		}
	}
	return nil
}

// handleResponse handles responses received from udp.
func handleResponse(dht *DHT, addr *net.UDPAddr, response map[string]interface{}) (success bool) {

	t := response["t"].(string)

	trans := dht.transacts.filterOne(t, addr)
	if trans == nil {
		return
	}

	// inform transManager to delete the transaction.
	if err := parseKey(response, "r", "map"); err != nil {
		return
	}

	q := trans.data["q"].(string)
	a := trans.data["a"].(map[string]interface{})
	r := response["r"].(map[string]interface{})

	if err := parseKey(r, "id", "string"); err != nil {
		return
	}

	id := r["id"].(string)

	if trans.tar.id != nil && trans.tar.id.RawString() != id {
		return
	}

	node, err := newNode(id, addr.Network(), addr.String())
	if err != nil {
		return
	}

	switch q {
	case pingType:
	case findNodeType:
		if trans.data["q"].(string) != findNodeType {
			return
		}

		target := trans.data["a"].(map[string]interface{})["target"].(string)
		if findOn(dht, r, newHashId(target), findNodeType) != nil {
			return
		}
	case getPeersType:
		if err := parseKey(r, "token", "string"); err != nil {
			return
		}

		token := r["token"].(string)
		infoHash := a["info_hash"].(string)

		if err := parseKey(r, "values", "list"); err == nil {
			values := r["values"].([]interface{})
			for _, v := range values {
				p, err := newPeerFromCompactIPPortInfo(v.(string), token)
				if err != nil {
					continue
				}
				dht.peers.Insert(infoHash, p)
			}
		} else if findOn(dht, r, newHashId(infoHash), getPeersType) != nil {
			return
		}
	case announcePeerType:
	default:
		return
	}

	// inform transManager to delete transaction.
	trans.response <- struct{}{}

	dht.rt.Insert(node)

	return true
}

// handleError handles errors received from udp.
func handleError(dht *DHT, addr *net.UDPAddr, response map[string]interface{}) (success bool) {

	if err := parseKey(response, "e", "list"); err != nil {
		return
	}

	if e := response["e"].([]interface{}); len(e) != 2 {
		return
	}

	if trans := dht.transacts.filterOne(response["t"].(string), addr); trans != nil {
		trans.response <- struct{}{}
	}

	return true
}

var handlers = map[string]func(*DHT, *net.UDPAddr, map[string]interface{}) bool{
	"q": handleRequest,
	"r": handleResponse,
	"e": handleError,
}

// handle handles packets received from udp.
func handle(dht *DHT, pkt packet) {
	select {
	case dht.works <- struct{}{}:
		go func() {
			defer func() {
				<-dht.works
			}()

			data := map[string]interface{}{}

			err := Decode(pkt.data, &data)
			if err != nil {
				return
			}

			response, err := parseMessage(data)
			if err != nil {
				return
			}

			if f, ok := handlers[response["y"].(string)]; ok {
				f(dht, pkt.raddr, response)
			}
		}()
	default:
		return
	}

}
