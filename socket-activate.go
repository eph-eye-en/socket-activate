package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/coreos/go-systemd/v22/activation"
	"github.com/coreos/go-systemd/v22/dbus"
)

var (
	mode               = flag.String("m", "tcp", "mode, accepting anything accepted by Go's net.Dial")
	targetUnit         = flag.String("u", "null.service", "corresponding unit")
	destinationAddress = flag.String("a", "127.0.0.1:80", "destination address, accepting anything accepted by Go's net.Dial")
	timeout            = flag.Duration("t", 0, "inactivity timeout after which to stop the unit again")
	retries            = flag.Uint("r", 10, "number of connection attempts (with 100ms delay) before giving up")
	user               = flag.Bool("user", false, "use user systemd rather than system")
)

type unitController struct {
	conn     *dbus.Conn
	unitname string
	ctx      context.Context
}

func newUnitController(name string, ctx context.Context) unitController {
	var conn *dbus.Conn
	var err error
	if *user {
		conn, err = dbus.NewUserConnectionContext(ctx)
	} else {
		conn, err = dbus.NewSystemConnectionContext(ctx)
	}
	if err != nil {
		log.Fatal(err)
	}
	return unitController{conn, name, ctx}
}

func (unitCtrl unitController) startSystemdUnit() {
	_, err := unitCtrl.conn.StartUnitContext(unitCtrl.ctx, unitCtrl.unitname, "replace", nil)
	if err != nil {
		log.Fatal(err)
	}
}

func (unitCtrl unitController) stopSystemdUnit() {
	_, err := unitCtrl.conn.StopUnitContext(unitCtrl.ctx, unitCtrl.unitname, "replace", nil)
	if err != nil {
		log.Fatal(err)
	}
}

func (unitCtrl unitController) cancelWithoutActivity(activity <-chan bool, cancel context.CancelFunc) {
	var timeoutCh <-chan time.Time
	if *timeout > 0 {
		timeoutCh = time.After(*timeout)
	}

	for {
		select {
		case <-activity:
		case <-timeoutCh:
			unitCtrl.stopSystemdUnit()
			cancel()
		}
	}
}

func proxyNetworkConnections(from net.Conn, to net.Conn, activityMonitor chan<- bool, cancel context.CancelFunc) {
	buffer := make([]byte, 1024)

	for {
		i, err := from.Read(buffer)
		if err != nil {
			cancel()
			return // EOF (if anything else, we scrap the connection anyways)
		}
		activityMonitor <- true
		to.Write(buffer[:i])
	}
}

func closeOnCancel(ctx context.Context, a net.Conn, b net.Conn) {
	<-ctx.Done()
	a.Close()
	b.Close()
}

func startTCPProxy(listener net.Listener, activityMonitor chan<- bool, ctx context.Context) {
	for {
		activityMonitor <- true
		connOutwards, err := listener.Accept()
		if err != nil {
			fmt.Println(err)
			return
		}

		var connBackend net.Conn
		var tryCount uint
		for tryCount = 0; tryCount < *retries; tryCount++ {
			connBackend, err = net.Dial(*mode, *destinationAddress)
			if err != nil {
				fmt.Println(err)
				time.Sleep(100 * time.Millisecond)
				continue
			} else {
				break
			}
		}
		if tryCount >= *retries {
			continue
		}

		ctx2, cancel := context.WithCancel(ctx)
		go proxyNetworkConnections(connOutwards, connBackend, activityMonitor, cancel)
		go proxyNetworkConnections(connBackend, connOutwards, activityMonitor, cancel)
		go closeOnCancel(ctx2, connOutwards, connBackend)
	}
}

func main() {

	flag.Parse()

	ls, err := activation.Listeners()
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	unitCtrl := newUnitController(*targetUnit, ctx)

	activityMonitor := make(chan bool)
	go unitCtrl.cancelWithoutActivity(activityMonitor, cancel)

	// first, connect to systemd for starting the unit
	unitCtrl.startSystemdUnit()

	// then take over the socket from systemd
	startTCPProxy(ls[0], activityMonitor, ctx)
}
