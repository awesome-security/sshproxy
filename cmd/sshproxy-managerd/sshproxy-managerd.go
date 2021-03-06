// Copyright 2015-2019 CEA/DAM/DIF
//  Contributor: Arnaud Guignard <arnaud.guignard@cea.fr>
//
// This software is governed by the CeCILL-B license under French law and
// abiding by the rules of distribution of free software.  You can  use,
// modify and/ or redistribute the software under the terms of the CeCILL-B
// license as circulated by CEA, CNRS and INRIA at the following URL
// "http://www.cecill.info".

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/cea-hpc/sshproxy/pkg/route"
	"github.com/cea-hpc/sshproxy/pkg/utils"

	"github.com/op/go-logging"
	"gopkg.in/yaml.v2"
)

var (
	// SshproxyVersion is set in the Makefile.
	SshproxyVersion   = "0.0.0+notproperlybuilt"
	defaultConfig     = "/etc/sshproxy/sshproxy-managerd.yaml"
	defaultListenAddr = "127.0.0.1:55555"
)

var (
	errInvalidHost        = errors.New("invalid host specified")
	errNotEnoughArguments = errors.New("not enough arguments")
)

var (
	// main logger for sshproxy-managerd
	log = logging.MustGetLogger("sshproxy-managerd")

	// configuration
	config managerdConfig

	// host checker keeping a pool of alive hosts.
	managerHostChecker = newHostChecker()

	// map of proxied connections (keys are user@host)
	proxiedConnections = make(map[string]*proxiedConn)

	findUserRegexp = regexp.MustCompile(`^(\w*)@`)
)

// Configuration
type managerdConfig struct {
	Debug         bool                 // Debug mode
	Listen        string               // Listen address [host]:port
	Log           string               // Where to log: empty is for stdout, "syslog" or a file
	CheckInterval utils.Duration       `yaml:"check_interval"` // Minimum interval between host checks
	RouteSelect   string               `yaml:"route_select"`   // Algorithm used to select a destination
	Routes        map[string][]string  // Routes definition
	Groups        map[string]subConfig // Groups overridden options
	Users         map[string]subConfig // Users overridden options
}

// sub-configuration for users/groups
type subConfig struct {
	RouteSelect string `yaml:"route_select"`
	Routes      map[string][]string
}

// loadConfig loads configuration from a file name and saves it in the config
// global variable.
func loadConfig(filename string) error {
	yamlFile, err := ioutil.ReadFile(filename)
	if err != nil {
		return err
	}

	if err := yaml.Unmarshal(yamlFile, &config); err != nil {
		return err
	}

	if len(config.Routes) == 0 {
		return fmt.Errorf("no routes specified")
	}

	if err := utils.CheckRoutes(config.Routes); err != nil {
		return fmt.Errorf("invalid value in `routes` option: %s", err)
	}

	for g, cfg := range config.Groups {
		if err := utils.CheckRoutes(cfg.Routes); err != nil {
			return fmt.Errorf("invalid value for group %s in `routes` option: %s", g, err)
		}
	}

	for u, cfg := range config.Users {
		if err := utils.CheckRoutes(cfg.Routes); err != nil {
			return fmt.Errorf("invalid value for user %s in `routes` option: %s", u, err)
		}
	}

	if config.Listen == "" {
		config.Listen = defaultListenAddr
	}

	if config.RouteSelect == "" {
		config.RouteSelect = route.DefaultAlgorithm
	}

	return nil
}

// State of an host
type State int

// These are the possible states of an host:
//   Up: the host is up,
//   Down: the host is down,
//   Disable: the host was disabled by an admin.
const (
	Up State = iota
	Down
	Disabled
)

var stateNames = map[State]string{
	Up:       "up",
	Down:     "down",
	Disabled: "disabled",
}

// hostState represents the result of an host check.
type hostState struct {
	State State     // host state (see State for available states)
	Ts    time.Time // time of last check
}

// hostChecker implements the sshproxy.route.HostChecker interface. It is used
// to keep a view of hosts state and to check their availability only after a
// defined duration (set in the config.CheckInterval global variable).
type hostChecker struct {
	States map[string]*hostState // map with "host:port" as keys and their associated state
}

// newHostChecker creates a new hostChecker.
func newHostChecker() *hostChecker {
	return &hostChecker{make(map[string]*hostState)}
}

// Check checks if an host is enabled and alive.
//
// It looks for it in its internal view. If found and its last check is less
// than config.CheckInterval duration it returns its known state. Otherwise it
// performs a check and updates (or adds a state to) the internal view
// accordingly.
func (hc *hostChecker) Check(hostport string) bool {
	ts := time.Now()
	var state State
	s, ok := hc.States[hostport]
	switch {
	case !ok:
		state = hc.DoCheck(hostport)
	case s.State == Disabled:
		state = s.State
	case ts.Sub(s.Ts) > config.CheckInterval.Duration():
		state = hc.DoCheck(hostport)
	default:
		state = s.State
	}
	return state == Up
}

// DoCheck checks if an host is alive and updates the internal view.
func (hc *hostChecker) DoCheck(hostport string) State {
	state := Down
	if route.CanConnect(hostport) {
		state = Up
	}
	hc.Update(hostport, state, time.Now())
	return state
}

// Update updates (or creates) the state of an host in the internal view.
func (hc *hostChecker) Update(hostport string, state State, ts time.Time) {
	if s, ok := hc.States[hostport]; ok {
		s.State = state
		s.Ts = ts
	} else {
		s = &hostState{
			State: state,
			Ts:    ts,
		}
		hc.States[hostport] = s
	}
}

// IsDisabled checks if an host is disabled.
func (hc *hostChecker) IsDisabled(hostport string) bool {
	if s, ok := hc.States[hostport]; ok {
		return s.State == Disabled
	}
	return false
}

// proxiedConn represents the details of a proxied connection for a couple
// (user, host).
type proxiedConn struct {
	Dest string    // Chosen destination
	N    int       // Number of connections
	Ts   time.Time // Start of last connection
}

// genKey returns the key used in the proxiedConnections global variable.
func genKey(user, host string) string {
	return fmt.Sprintf("%s@%s", user, host)
}

// getUserFromKey returns the user from the key used in the proxiedConnections global variable.
func getUserFromKey(key string) (string, error) {
	match := findUserRegexp.FindStringSubmatch(key)
	if len(match) < 2 {
		return "", fmt.Errorf("Unable to extract user from given key (%s)", key)
	}
	return match[1], nil
}

// getAlgorithmAndRoutes returns the selection algorithm and a slice with the
// possible destinations from the global configuration for a user connected to
// an hostport and belonging to groups.
func getAlgorithmAndRoutes(user, hostport string, groups map[string]bool) (string, []string) {
	configs := []*subConfig{}

	// add main config
	configs = append(configs, &subConfig{RouteSelect: config.RouteSelect, Routes: config.Routes})
	// add group configs
	for g, cfg := range config.Groups {
		if groups[g] {
			configs = append(configs, &cfg)
		}
	}
	// add user config
	if cfg, ok := config.Users[user]; ok {
		configs = append(configs, &cfg)
	}

	algo := ""
	dests := []string{}

	for _, cfg := range configs {
		if cfg.RouteSelect != "" {
			algo = cfg.RouteSelect
		}
		if d, ok := cfg.Routes[hostport]; ok {
			dests = d
		} else if d, ok := cfg.Routes[route.DefaultRouteKeyword]; ok {
			dests = d
		}
	}

	return algo, dests
}

// selectRoute returns a destination for a user connected to an hostport. The
// destination is available before returning it.
func selectRoute(user, hostport string) (string, error) {
	groups, err := utils.GetGroupList(user)
	if err != nil {
		return "", fmt.Errorf("cannot find groups for user '%s': %s", user, err)
	}

	algo, dests := getAlgorithmAndRoutes(user, hostport, groups)

	dst, err := route.Select(algo, dests, managerHostChecker)
	if err != nil {
		return "", fmt.Errorf("cannot select route for user '%s': %s", user, err)
	}

	return dst, nil
}

func checkHostPort(hostport string) (string, error) {
	host, port, err := utils.SplitHostPort(hostport)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, port), nil
}

// commandHandler represents an handler for a protocol command.
type commandHandler func([]string) (string, error)

// commandHandlers associates available handlers for known commands.
var commandHandlers = map[string]commandHandler{
	"connect":    connectHandler,
	"disable":    disableHandler,
	"disconnect": disconnectHandler,
	"enable":     enableHandler,
	"info":       infoHandler,
	"failure":    failureHandler,
}

// The protocol for communicating with the managerd is simple and based on the
// Redis protocol (http://redis.io/topics/protocol):
//   - all commands and responses are terminated with CRLF
//   - the client sends an ASCII command
//   - the server ASCII response begins with:
//     * '+' followed by a string for simple strings
//     * '-' followed by an error message in case of error
//     * '$' for bulk strings (i.e. strings with CRLF or binary data):
//       . the '$' is followed by the number of bytes of the string terminated with CRLF
//       . the string itself
//       . the mandatory CRLF
//       For example: '$6\r\nHELLO!\r\n' (which could also be sent as '+HELLO!\r\n')
//       A bulk string can also be used to represent a NULL value when the
//       length is -1: '$-1\r\n'.

// connectHandler handles the "connect user host[:port]" command.
//
// It returns a destination (which can be empty) or an error message.
func connectHandler(args []string) (string, error) {
	if len(args) != 2 {
		return "", errNotEnoughArguments
	}

	user := args[0]
	hostport, err := checkHostPort(args[1])
	if err != nil {
		return "", errInvalidHost
	}

	key := genKey(user, hostport)
	pc, ok := proxiedConnections[key]
	if ok {
		log.Infof("found connection for %s: %s", key, pc.Dest)
		if managerHostChecker.Check(pc.Dest) {
			pc.N++
			pc.Ts = time.Now()
			return fmt.Sprintf("+%s", pc.Dest), nil
		}
		log.Infof("cannot connect %s to already existing connection(s) to %s: host down or disabled", key, pc.Dest)
		if !managerHostChecker.IsDisabled(pc.Dest) {
			managerHostChecker.Update(pc.Dest, Down, time.Now())
		}
	}

	dst, err := selectRoute(user, hostport)
	if err != nil {
		return "", err
	}

	if dst == "" {
		log.Warningf("no valid or available connection found for %s", key)
		return "$-1\r\n", nil
	}

	proxiedConnections[key] = &proxiedConn{
		Dest: dst,
		N:    1,
		Ts:   time.Now(),
	}

	log.Infof("new connection for %s: %s", key, dst)
	return fmt.Sprintf("+%s", dst), nil
}

// disableHandler handles the "disable host[:port] command.
//
// It returns "+OK" or an error message.
func disableHandler(args []string) (string, error) {
	if len(args) != 1 {
		return "", errNotEnoughArguments
	}

	hostport, err := checkHostPort(args[0])
	if err != nil {
		return "", errInvalidHost
	}

	managerHostChecker.Update(hostport, Disabled, time.Now())

	return "+OK", nil
}

// disconnectHandler handles the "disconnect user host[:port]" command.
//
// It returns "+OK" or an error message.
func disconnectHandler(args []string) (string, error) {
	if len(args) != 2 {
		return "", errNotEnoughArguments
	}

	user := args[0]
	hostport, err := checkHostPort(args[1])
	if err != nil {
		return "", errInvalidHost
	}

	key := genKey(user, hostport)
	pc, ok := proxiedConnections[key]
	if !ok {
		return "+OK", fmt.Errorf("key is not present: %s", key)
	}

	pc.N--
	if pc.N == 0 {
		log.Infof("no more active connection for %s (to %s): removing", key, pc.Dest)
		delete(proxiedConnections, key)
	}

	return "+OK", nil
}

// enableHandler handles the "enable host[:port]" command.
//
// It returns "+OK" or an error message.
func enableHandler(args []string) (string, error) {
	if len(args) != 1 {
		return "", errNotEnoughArguments
	}

	hostport, err := checkHostPort(args[0])
	if err != nil {
		return "", errInvalidHost
	}

	if managerHostChecker.IsDisabled(hostport) {
		managerHostChecker.DoCheck(hostport)
	} else {
		log.Warningf("host %s is already enabled", hostport)
	}

	return "+OK", nil
}

// infoHandler handles the "info (connections|checks)" command.
//
// It returns the infos or an error message.
func infoHandler(args []string) (string, error) {
	if len(args) == 0 {
		return "", errNotEnoughArguments
	}

	var lines []string
	switch strings.ToLower(args[0]) {
	case "connections":
		lines = make([]string, len(proxiedConnections))
		i := 0
		for k, v := range proxiedConnections {
			lines[i] = fmt.Sprintf("id=%s dest=%s n=%d ts=%s", k, v.Dest, v.N, v.Ts.Format(time.RFC3339Nano))
			i++
		}
	case "checks":
		lines = make([]string, len(managerHostChecker.States))
		i := 0
		for k, v := range managerHostChecker.States {
			lines[i] = fmt.Sprintf("host=%s state=%s ts=%s", k, stateNames[v.State], v.Ts.Format(time.RFC3339Nano))
			i++
		}
	default:
		return "", fmt.Errorf("unknown parameter: %s", args[0])
	}

	msg := strings.Join(lines, "\r\n")
	return fmt.Sprintf("$%d\r\n%s", len(msg), msg), nil
}

// failureHandler handles the "failure host[:port]" command.
//
// It returns "+OK" or an error message.
func failureHandler(args []string) (string, error) {
	if len(args) != 1 {
		return "", errNotEnoughArguments
	}

	hostport, err := checkHostPort(args[0])
	if err != nil {
		return "", errInvalidHost
	}

	// Check host before marking it down
	if !managerHostChecker.IsDisabled(hostport) {
		if managerHostChecker.DoCheck(hostport) == Up {
			return "+OK", fmt.Errorf("%s is alive", hostport)
		}
	} else {
		return "+OK", fmt.Errorf("%s is disabled", hostport)
	}

	return "+OK", nil
}

// request represents a request from a client.
type request struct {
	request  string      // the request sent by the client
	errc     chan error  // channel to write a possible error
	response chan string // channel to write a possible response
}

// handle processes a request from a client.
//
// It either writes a response in the request.response channel or an error in
// the request.errc channel.
func handle(r *request) {
	fields := strings.Fields(r.request)
	if len(fields) == 0 {
		r.errc <- errors.New("empty request")
		return
	}

	command := strings.ToLower(fields[0])

	handler, ok := commandHandlers[command]
	if !ok {
		r.errc <- fmt.Errorf("unknown command '%s'", command)
		return
	}

	response, err := handler(fields[1:])
	if err != nil {
		r.errc <- err
		return
	}

	r.response <- response
	close(r.response)
}

// serve processes requests written in the queue channel and quits when the
// context is cancelled.
func serve(ctx context.Context, queue <-chan *request) {
	for {
		select {
		case req := <-queue:
			handle(req)
		case <-ctx.Done():
			return
		}
	}
}

// formatError returns an error message string according to sshproxy-managerd
// protocol (i.e. '-ERR error message')
func formatError(err error) string {
	return fmt.Sprintf("-ERR %s", err)
}

// writeResponse writes a response to a client.
func writeResponse(c net.Conn, response string) {
	writer := bufio.NewWriter(c)
	writer.WriteString(response)
	writer.WriteString("\r\n")
	writer.Flush()
}

// acquire reads a command from a client, writes the request to the queue
// channel and waits for a response or an error.
//
// Only a valid response is sent back to the client, i.e. if there is an error
// the connection is just closed without a message.
func acquire(c net.Conn, queue chan *request) {
	defer c.Close()

	addr := c.RemoteAddr()
	log.Debugf("connection from %s", addr)
	defer log.Debugf("disconnection from %s", addr)

	reader := bufio.NewReader(c)
	req, err := reader.ReadString('\n')
	if err != nil {
		log.Errorf("reading from %s: %s", addr, err)
		return
	}

	req = strings.TrimSpace(req)
	log.Debugf("request = %s", req)

	r := &request{
		request:  req,
		errc:     make(chan error, 1),
		response: make(chan string, 1),
	}

	queue <- r

	select {
	case err := <-r.errc:
		log.Errorf("handling request '%s' from %s: %s", req, addr, err)
		writeResponse(c, formatError(err))
		return
	case response := <-r.response:
		if response != "" {
			writeResponse(c, response)
		}
	}
}

// usage of program.
func usage() {
	fmt.Fprintf(os.Stderr, "usage: sshproxy-managerd [config]\n")
	flag.PrintDefaults()
	os.Exit(2)
}

func main() {
	versionFlag := flag.Bool("version", false, "show version number and exit")
	flag.Usage = usage
	flag.Parse()

	if *versionFlag {
		fmt.Fprintf(os.Stderr, "sshproxy-managerd version %s\n", SshproxyVersion)
		os.Exit(0)
	}

	configFile := defaultConfig
	if flag.NArg() != 0 {
		configFile = flag.Arg(0)
	}

	if err := loadConfig(configFile); err != nil {
		log.Fatalf("ERROR: reading configuration '%s': %s", configFile, err)
	}

	logformat := "%{time:2006-01-02 15:04:05} %{level}: %{message}"
	syslogformat := "%{level}: %{message}"
	utils.MustSetupLogging("sshproxy-managerd", config.Log, logformat, syslogformat, config.Debug)

	log.Debugf("config.debug = %v", config.Debug)
	log.Debugf("config.listen = %s", config.Listen)
	log.Debugf("config.log = %s", config.Log)
	log.Debugf("config.check_interval = %s", config.CheckInterval.Duration())
	log.Debugf("config.route_select = %s", config.RouteSelect)
	log.Debugf("config.routes = %v", config.Routes)
	log.Debugf("config.groups = %v", config.Groups)
	log.Debugf("config.users = %v", config.Users)

	// Ignore SIGPIPE events.
	// They are generated by systemd when journald is restarted and
	// sshproxy-managerd is running under systemd, logging to stdout and
	// not restarted.
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGPIPE)
	go func() {
		for {
			<-c
			// Switch logging to syslog.
			// With systemd >= 219 it should not be necessary but
			// Red Hat reverted the needed functionality from the
			// systemd used on RHEL 7.x:
			// Patch:  0125-Revert-journald-allow-restarting-journald-without-lo.patch
			// Reason: https://lists.freedesktop.org/archives/systemd-devel/2015-February/028685.html
			if config.Log == "" {
				config.Log = "syslog"
				utils.MustSetupLogging("sshproxy-managerd", "syslog", logformat, syslogformat, config.Debug)
			}
			log.Debug("Received SIGPIPE")
		}
	}()

	l, err := net.Listen("tcp", config.Listen)
	if err != nil {
		log.Fatalf("error: listening: %s", err)
	}
	defer l.Close()

	log.Infof("listening on %s", config.Listen)

	queue := make(chan *request)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go serve(ctx, queue)

	for {
		conn, err := l.Accept()
		if err != nil {
			log.Fatalf("error: accepting connection: %s", err)
		}

		go acquire(conn, queue)
	}
}
