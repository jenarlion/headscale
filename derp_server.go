package headscale

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
	"tailscale.com/derp"
	"tailscale.com/net/stun"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// fastStartHeader is the header (with value "1") that signals to the HTTP
// server that the DERP HTTP client does not want the HTTP 101 response
// headers and it will begin writing & reading the DERP protocol immediately
// following its HTTP request.
const fastStartHeader = "Derp-Fast-Start"

var (
	dnsCache     atomic.Value // of []byte
	bootstrapDNS = "derp.tailscale.com"
)

type DERPServer struct {
	tailscaleDERP *derp.Server
	region        tailcfg.DERPRegion
}

func (h *Headscale) NewDERPServer() (*DERPServer, error) {
	s := derp.NewServer(key.NodePrivate(*h.privateKey), log.Info().Msgf)
	region, err := h.generateRegionLocalDERP()
	if err != nil {
		return nil, err
	}
	return &DERPServer{s, region}, nil
}

func (h *Headscale) generateRegionLocalDERP() (tailcfg.DERPRegion, error) {
	serverURL, err := url.Parse(h.cfg.ServerURL)
	if err != nil {
		return tailcfg.DERPRegion{}, err
	}
	var host string
	var port int
	host, portStr, err := net.SplitHostPort(serverURL.Host)
	if err != nil {
		if serverURL.Scheme == "https" {
			host = serverURL.Host
			port = 443
		} else {
			host = serverURL.Host
			port = 80
		}
	} else {
		port, err = strconv.Atoi(portStr)
		if err != nil {
			return tailcfg.DERPRegion{}, err
		}
	}

	localDERPregion := tailcfg.DERPRegion{
		RegionID:   999,
		RegionCode: "headscale",
		RegionName: "Headscale Embedded DERP",
		Avoid:      false,
		Nodes: []*tailcfg.DERPNode{
			{
				Name:     "999a",
				RegionID: 999,
				HostName: host,
				DERPPort: port,
			},
		},
	}
	return localDERPregion, nil
}

func (h *Headscale) DERPHandler(ctx *gin.Context) {
	log.Trace().Caller().Msgf("/derp request from %v", ctx.ClientIP())
	up := strings.ToLower(ctx.Request.Header.Get("Upgrade"))
	if up != "websocket" && up != "derp" {
		if up != "" {
			log.Warn().Caller().Msgf("Weird websockets connection upgrade: %q", up)
		}
		ctx.String(http.StatusUpgradeRequired, "DERP requires connection upgrade")
		return
	}

	fastStart := ctx.Request.Header.Get(fastStartHeader) == "1"

	hijacker, ok := ctx.Writer.(http.Hijacker)
	if !ok {
		log.Error().Caller().Msg("DERP requires Hijacker interface from Gin")
		ctx.String(http.StatusInternalServerError, "HTTP does not support general TCP support")
		return
	}

	netConn, conn, err := hijacker.Hijack()
	if err != nil {
		log.Error().Caller().Err(err).Msgf("Hijack failed")
		ctx.String(http.StatusInternalServerError, "HTTP does not support general TCP support")
		return
	}

	if !fastStart {
		pubKey := h.privateKey.Public()
		fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: DERP\r\n"+
			"Connection: Upgrade\r\n"+
			"Derp-Version: %v\r\n"+
			"Derp-Public-Key: %s\r\n\r\n",
			derp.ProtocolVersion,
			pubKey.UntypedHexString())
	}

	h.DERPServer.tailscaleDERP.Accept(netConn, conn, netConn.RemoteAddr().String())
}

// DERPProbeHandler is the endpoint that js/wasm clients hit to measure
// DERP latency, since they can't do UDP STUN queries.
func (h *Headscale) DERPProbeHandler(ctx *gin.Context) {
	switch ctx.Request.Method {
	case "HEAD", "GET":
		ctx.Writer.Header().Set("Access-Control-Allow-Origin", "*")
	default:
		ctx.String(http.StatusMethodNotAllowed, "bogus probe method")
	}
}

func (h *Headscale) DERPBootstrapDNSHandler(ctx *gin.Context) {
	ctx.Header("Content-Type", "application/json")
	j, _ := dnsCache.Load().([]byte)
	// Bootstrap DNS requests occur cross-regions,
	// and are randomized per request,
	// so keeping a connection open is pointlessly expensive.
	ctx.Header("Connection", "close")
	ctx.Writer.Write(j)
}

// ServeSTUN starts a STUN server on udp/3478
func (h *Headscale) ServeSTUN() {
	pc, err := net.ListenPacket("udp", "0.0.0.0:3478")
	if err != nil {
		log.Fatal().Msgf("failed to open STUN listener: %v", err)
	}
	log.Trace().Msgf("STUN server started at %s", pc.LocalAddr())
	serverSTUNListener(context.Background(), pc.(*net.UDPConn))
}

func serverSTUNListener(ctx context.Context, pc *net.UDPConn) {
	var buf [64 << 10]byte
	var (
		n   int
		ua  *net.UDPAddr
		err error
	)
	for {
		n, ua, err = pc.ReadFromUDP(buf[:])
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error().Caller().Err(err).Msgf("STUN ReadFrom")
			time.Sleep(time.Second)
			continue
		}
		log.Trace().Caller().Msgf("STUN request from %v", ua)
		pkt := buf[:n]
		if !stun.Is(pkt) {
			continue
		}
		txid, err := stun.ParseBindingRequest(pkt)
		if err != nil {
			continue
		}

		res := stun.Response(txid, ua.IP, uint16(ua.Port))
		pc.WriteTo(res, ua)
	}
}

// Shamelessly taken from
// https://github.com/tailscale/tailscale/blob/main/cmd/derper/bootstrap_dns.go
func refreshBootstrapDNSLoop() {
	if bootstrapDNS == "" {
		return
	}
	for {
		refreshBootstrapDNS()
		time.Sleep(10 * time.Minute)
	}
}

func refreshBootstrapDNS() {
	if bootstrapDNS == "" {
		return
	}
	dnsEntries := make(map[string][]net.IP)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	names := strings.Split(bootstrapDNS, ",")
	var r net.Resolver
	for _, name := range names {
		addrs, err := r.LookupIP(ctx, "ip", name)
		if err != nil {
			log.Trace().Caller().Err(err).Msgf("bootstrap DNS lookup %q", name)
			continue
		}
		dnsEntries[name] = addrs
	}
	j, err := json.MarshalIndent(dnsEntries, "", "\t")
	if err != nil {
		// leave the old values in place
		return
	}
	dnsCache.Store(j)
}
