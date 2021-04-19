// Copyright 2020 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package forward_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checker"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/tests/utils"
	"gvisor.dev/gvisor/pkg/tcpip/testutil"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

func TestForwarding(t *testing.T) {
	const listenPort = 8080

	type endpointAndAddresses struct {
		serverEP         tcpip.Endpoint
		serverAddr       tcpip.Address
		serverReadableCH chan struct{}

		clientEP         tcpip.Endpoint
		clientAddr       tcpip.Address
		clientReadableCH chan struct{}
	}

	newEP := func(t *testing.T, s *stack.Stack, transProto tcpip.TransportProtocolNumber, netProto tcpip.NetworkProtocolNumber) (tcpip.Endpoint, chan struct{}) {
		t.Helper()
		var wq waiter.Queue
		we, ch := waiter.NewChannelEntry(nil)
		wq.EventRegister(&we, waiter.ReadableEvents)
		ep, err := s.NewEndpoint(transProto, netProto, &wq)
		if err != nil {
			t.Fatalf("s.NewEndpoint(%d, %d, _): %s", transProto, netProto, err)
		}

		t.Cleanup(func() {
			wq.EventUnregister(&we)
		})

		return ep, ch
	}

	tests := []struct {
		name       string
		epAndAddrs func(t *testing.T, host1Stack, routerStack, host2Stack *stack.Stack, proto tcpip.TransportProtocolNumber) endpointAndAddresses
	}{
		{
			name: "IPv4 host1 server with host2 client",
			epAndAddrs: func(t *testing.T, host1Stack, routerStack, host2Stack *stack.Stack, proto tcpip.TransportProtocolNumber) endpointAndAddresses {
				ep1, ep1WECH := newEP(t, host1Stack, proto, ipv4.ProtocolNumber)
				ep2, ep2WECH := newEP(t, host2Stack, proto, ipv4.ProtocolNumber)
				return endpointAndAddresses{
					serverEP:         ep1,
					serverAddr:       utils.Host1IPv4Addr.AddressWithPrefix.Address,
					serverReadableCH: ep1WECH,

					clientEP:         ep2,
					clientAddr:       utils.Host2IPv4Addr.AddressWithPrefix.Address,
					clientReadableCH: ep2WECH,
				}
			},
		},
		{
			name: "IPv6 host2 server with host1 client",
			epAndAddrs: func(t *testing.T, host1Stack, routerStack, host2Stack *stack.Stack, proto tcpip.TransportProtocolNumber) endpointAndAddresses {
				ep1, ep1WECH := newEP(t, host2Stack, proto, ipv6.ProtocolNumber)
				ep2, ep2WECH := newEP(t, host1Stack, proto, ipv6.ProtocolNumber)
				return endpointAndAddresses{
					serverEP:         ep1,
					serverAddr:       utils.Host2IPv6Addr.AddressWithPrefix.Address,
					serverReadableCH: ep1WECH,

					clientEP:         ep2,
					clientAddr:       utils.Host1IPv6Addr.AddressWithPrefix.Address,
					clientReadableCH: ep2WECH,
				}
			},
		},
		{
			name: "IPv4 host2 server with routerNIC1 client",
			epAndAddrs: func(t *testing.T, host1Stack, routerStack, host2Stack *stack.Stack, proto tcpip.TransportProtocolNumber) endpointAndAddresses {
				ep1, ep1WECH := newEP(t, host2Stack, proto, ipv4.ProtocolNumber)
				ep2, ep2WECH := newEP(t, routerStack, proto, ipv4.ProtocolNumber)
				return endpointAndAddresses{
					serverEP:         ep1,
					serverAddr:       utils.Host2IPv4Addr.AddressWithPrefix.Address,
					serverReadableCH: ep1WECH,

					clientEP:         ep2,
					clientAddr:       utils.RouterNIC1IPv4Addr.AddressWithPrefix.Address,
					clientReadableCH: ep2WECH,
				}
			},
		},
		{
			name: "IPv6 routerNIC2 server with host1 client",
			epAndAddrs: func(t *testing.T, host1Stack, routerStack, host2Stack *stack.Stack, proto tcpip.TransportProtocolNumber) endpointAndAddresses {
				ep1, ep1WECH := newEP(t, routerStack, proto, ipv6.ProtocolNumber)
				ep2, ep2WECH := newEP(t, host1Stack, proto, ipv6.ProtocolNumber)
				return endpointAndAddresses{
					serverEP:         ep1,
					serverAddr:       utils.RouterNIC2IPv6Addr.AddressWithPrefix.Address,
					serverReadableCH: ep1WECH,

					clientEP:         ep2,
					clientAddr:       utils.Host1IPv6Addr.AddressWithPrefix.Address,
					clientReadableCH: ep2WECH,
				}
			},
		},
	}

	subTests := []struct {
		name               string
		proto              tcpip.TransportProtocolNumber
		expectedConnectErr tcpip.Error
		setupServer        func(t *testing.T, ep tcpip.Endpoint)
		setupServerConn    func(t *testing.T, ep tcpip.Endpoint, ch <-chan struct{}, clientAddr tcpip.FullAddress) (tcpip.Endpoint, chan struct{})
		needRemoteAddr     bool
	}{
		{
			name:               "UDP",
			proto:              udp.ProtocolNumber,
			expectedConnectErr: nil,
			setupServerConn: func(t *testing.T, ep tcpip.Endpoint, _ <-chan struct{}, clientAddr tcpip.FullAddress) (tcpip.Endpoint, chan struct{}) {
				t.Helper()

				if err := ep.Connect(clientAddr); err != nil {
					t.Fatalf("ep.Connect(%#v): %s", clientAddr, err)
				}
				return nil, nil
			},
			needRemoteAddr: true,
		},
		{
			name:               "TCP",
			proto:              tcp.ProtocolNumber,
			expectedConnectErr: &tcpip.ErrConnectStarted{},
			setupServer: func(t *testing.T, ep tcpip.Endpoint) {
				t.Helper()

				if err := ep.Listen(1); err != nil {
					t.Fatalf("ep.Listen(1): %s", err)
				}
			},
			setupServerConn: func(t *testing.T, ep tcpip.Endpoint, ch <-chan struct{}, clientAddr tcpip.FullAddress) (tcpip.Endpoint, chan struct{}) {
				t.Helper()

				var addr tcpip.FullAddress
				for {
					newEP, wq, err := ep.Accept(&addr)
					if _, ok := err.(*tcpip.ErrWouldBlock); ok {
						<-ch
						continue
					}
					if err != nil {
						t.Fatalf("ep.Accept(_): %s", err)
					}
					if diff := cmp.Diff(clientAddr, addr, checker.IgnoreCmpPath(
						"NIC",
					)); diff != "" {
						t.Errorf("accepted address mismatch (-want +got):\n%s", diff)
					}

					we, newCH := waiter.NewChannelEntry(nil)
					wq.EventRegister(&we, waiter.ReadableEvents)
					return newEP, newCH
				}
			},
			needRemoteAddr: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, subTest := range subTests {
				t.Run(subTest.name, func(t *testing.T) {
					stackOpts := stack.Options{
						NetworkProtocols:   []stack.NetworkProtocolFactory{arp.NewProtocol, ipv4.NewProtocol, ipv6.NewProtocol},
						TransportProtocols: []stack.TransportProtocolFactory{udp.NewProtocol, tcp.NewProtocol},
					}

					host1Stack := stack.New(stackOpts)
					routerStack := stack.New(stackOpts)
					host2Stack := stack.New(stackOpts)
					utils.SetupRoutedStacks(t, host1Stack, routerStack, host2Stack)

					epsAndAddrs := test.epAndAddrs(t, host1Stack, routerStack, host2Stack, subTest.proto)
					defer epsAndAddrs.serverEP.Close()
					defer epsAndAddrs.clientEP.Close()

					serverAddr := tcpip.FullAddress{Addr: epsAndAddrs.serverAddr, Port: listenPort}
					if err := epsAndAddrs.serverEP.Bind(serverAddr); err != nil {
						t.Fatalf("epsAndAddrs.serverEP.Bind(%#v): %s", serverAddr, err)
					}
					clientAddr := tcpip.FullAddress{Addr: epsAndAddrs.clientAddr}
					if err := epsAndAddrs.clientEP.Bind(clientAddr); err != nil {
						t.Fatalf("epsAndAddrs.clientEP.Bind(%#v): %s", clientAddr, err)
					}

					if subTest.setupServer != nil {
						subTest.setupServer(t, epsAndAddrs.serverEP)
					}
					{
						err := epsAndAddrs.clientEP.Connect(serverAddr)
						if diff := cmp.Diff(subTest.expectedConnectErr, err); diff != "" {
							t.Fatalf("unexpected error from epsAndAddrs.clientEP.Connect(%#v), (-want, +got):\n%s", serverAddr, diff)
						}
					}
					if addr, err := epsAndAddrs.clientEP.GetLocalAddress(); err != nil {
						t.Fatalf("epsAndAddrs.clientEP.GetLocalAddress(): %s", err)
					} else {
						clientAddr = addr
						clientAddr.NIC = 0
					}

					serverEP := epsAndAddrs.serverEP
					serverCH := epsAndAddrs.serverReadableCH
					if ep, ch := subTest.setupServerConn(t, serverEP, serverCH, clientAddr); ep != nil {
						defer ep.Close()
						serverEP = ep
						serverCH = ch
					}

					write := func(ep tcpip.Endpoint, data []byte) {
						t.Helper()

						var r bytes.Reader
						r.Reset(data)
						var wOpts tcpip.WriteOptions
						n, err := ep.Write(&r, wOpts)
						if err != nil {
							t.Fatalf("ep.Write(_, %#v): %s", wOpts, err)
						}
						if want := int64(len(data)); n != want {
							t.Fatalf("got ep.Write(_, %#v) = (%d, _), want = (%d, _)", wOpts, n, want)
						}
					}

					data := []byte{1, 2, 3, 4}
					write(epsAndAddrs.clientEP, data)

					read := func(ch chan struct{}, ep tcpip.Endpoint, data []byte, expectedFrom tcpip.FullAddress) {
						t.Helper()

						var buf bytes.Buffer
						var res tcpip.ReadResult
						for {
							var err tcpip.Error
							opts := tcpip.ReadOptions{NeedRemoteAddr: subTest.needRemoteAddr}
							res, err = ep.Read(&buf, opts)
							if _, ok := err.(*tcpip.ErrWouldBlock); ok {
								<-ch
								continue
							}
							if err != nil {
								t.Fatalf("ep.Read(_, %d, %#v): %s", len(data), opts, err)
							}
							break
						}

						readResult := tcpip.ReadResult{
							Count: len(data),
							Total: len(data),
						}
						if subTest.needRemoteAddr {
							readResult.RemoteAddr = expectedFrom
						}
						if diff := cmp.Diff(readResult, res, checker.IgnoreCmpPath(
							"ControlMessages",
							"RemoteAddr.NIC",
						)); diff != "" {
							t.Errorf("ep.Read: unexpected result (-want +got):\n%s", diff)
						}
						if diff := cmp.Diff(buf.Bytes(), data); diff != "" {
							t.Errorf("received data mismatch (-want +got):\n%s", diff)
						}

						if t.Failed() {
							t.FailNow()
						}
					}

					read(serverCH, serverEP, data, clientAddr)

					data = []byte{5, 6, 7, 8, 9, 10, 11, 12}
					write(serverEP, data)
					read(epsAndAddrs.clientReadableCH, epsAndAddrs.clientEP, data, serverAddr)
				})
			}
		})
	}
}

func TestMulticastForwarding(t *testing.T) {
	const (
		nicID1 = 1
		nicID2 = 2
		ttl    = 64
	)

	var (
		ipv4LinkLocalUnicastAddr   = testutil.MustParse4("169.254.0.10")
		ipv4LinkLocalMulticastAddr = testutil.MustParse4("224.0.0.10")
		ipv4GlobalMulticastAddr    = testutil.MustParse4("224.0.1.10")

		ipv6LinkLocalUnicastAddr   = testutil.MustParse6("fe80::a")
		ipv6LinkLocalMulticastAddr = testutil.MustParse6("ff02::a")
		ipv6GlobalMulticastAddr    = testutil.MustParse6("ff0e::a")
	)

	rxICMPv4EchoRequest := func(e *channel.Endpoint, src, dst tcpip.Address) {
		utils.RxICMPv4EchoRequest(e, src, dst, ttl)
	}

	rxICMPv6EchoRequest := func(e *channel.Endpoint, src, dst tcpip.Address) {
		utils.RxICMPv6EchoRequest(e, src, dst, ttl)
	}

	v4Checker := func(t *testing.T, b []byte, src, dst tcpip.Address) {
		checker.IPv4(t, b,
			checker.SrcAddr(src),
			checker.DstAddr(dst),
			checker.TTL(ttl-1),
			checker.ICMPv4(
				checker.ICMPv4Type(header.ICMPv4Echo)))
	}

	v6Checker := func(t *testing.T, b []byte, src, dst tcpip.Address) {
		checker.IPv6(t, b,
			checker.SrcAddr(src),
			checker.DstAddr(dst),
			checker.TTL(ttl-1),
			checker.ICMPv6(
				checker.ICMPv6Type(header.ICMPv6EchoRequest)))
	}

	tests := []struct {
		name             string
		srcAddr, dstAddr tcpip.Address
		rx               func(*channel.Endpoint, tcpip.Address, tcpip.Address)
		expectForward    bool
		checker          func(*testing.T, []byte)
	}{
		{
			name:          "IPv4 link-local multicast destination",
			srcAddr:       utils.RemoteIPv4Addr,
			dstAddr:       ipv4LinkLocalMulticastAddr,
			rx:            rxICMPv4EchoRequest,
			expectForward: false,
		},
		{
			name:          "IPv4 link-local source",
			srcAddr:       ipv4LinkLocalUnicastAddr,
			dstAddr:       utils.RemoteIPv4Addr,
			rx:            rxICMPv4EchoRequest,
			expectForward: false,
		},
		{
			name:          "IPv4 link-local destination",
			srcAddr:       utils.RemoteIPv4Addr,
			dstAddr:       ipv4LinkLocalUnicastAddr,
			rx:            rxICMPv4EchoRequest,
			expectForward: false,
		},
		{
			name:          "IPv4 non-link-local unicast",
			srcAddr:       utils.RemoteIPv4Addr,
			dstAddr:       utils.Ipv4Addr2.AddressWithPrefix.Address,
			rx:            rxICMPv4EchoRequest,
			expectForward: true,
			checker: func(t *testing.T, b []byte) {
				v4Checker(t, b, utils.RemoteIPv4Addr, utils.Ipv4Addr2.AddressWithPrefix.Address)
			},
		},
		{
			name:          "IPv4 non-link-local multicast",
			srcAddr:       utils.RemoteIPv4Addr,
			dstAddr:       ipv4GlobalMulticastAddr,
			rx:            rxICMPv4EchoRequest,
			expectForward: true,
			checker: func(t *testing.T, b []byte) {
				v4Checker(t, b, utils.RemoteIPv4Addr, ipv4GlobalMulticastAddr)
			},
		},

		{
			name:          "IPv6 link-local multicast destination",
			srcAddr:       utils.RemoteIPv6Addr,
			dstAddr:       ipv6LinkLocalMulticastAddr,
			rx:            rxICMPv6EchoRequest,
			expectForward: false,
		},
		{
			name:          "IPv6 link-local source",
			srcAddr:       ipv6LinkLocalUnicastAddr,
			dstAddr:       utils.RemoteIPv6Addr,
			rx:            rxICMPv6EchoRequest,
			expectForward: false,
		},
		{
			name:          "IPv6 link-local destination",
			srcAddr:       utils.RemoteIPv6Addr,
			dstAddr:       ipv6LinkLocalUnicastAddr,
			rx:            rxICMPv6EchoRequest,
			expectForward: false,
		},
		{
			name:          "IPv6 non-link-local unicast",
			srcAddr:       utils.RemoteIPv6Addr,
			dstAddr:       utils.Ipv6Addr2.AddressWithPrefix.Address,
			rx:            rxICMPv6EchoRequest,
			expectForward: true,
			checker: func(t *testing.T, b []byte) {
				v6Checker(t, b, utils.RemoteIPv6Addr, utils.Ipv6Addr2.AddressWithPrefix.Address)
			},
		},
		{
			name:          "IPv6 non-link-local multicast",
			srcAddr:       utils.RemoteIPv6Addr,
			dstAddr:       ipv6GlobalMulticastAddr,
			rx:            rxICMPv6EchoRequest,
			expectForward: true,
			checker: func(t *testing.T, b []byte) {
				v6Checker(t, b, utils.RemoteIPv6Addr, ipv6GlobalMulticastAddr)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := stack.New(stack.Options{
				NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
				TransportProtocols: []stack.TransportProtocolFactory{udp.NewProtocol},
			})

			e1 := channel.New(1, header.IPv6MinimumMTU, "")
			if err := s.CreateNIC(nicID1, e1); err != nil {
				t.Fatalf("s.CreateNIC(%d, _): %s", nicID1, err)
			}

			e2 := channel.New(1, header.IPv6MinimumMTU, "")
			if err := s.CreateNIC(nicID2, e2); err != nil {
				t.Fatalf("s.CreateNIC(%d, _): %s", nicID2, err)
			}

			if err := s.AddAddress(nicID2, ipv4.ProtocolNumber, utils.Ipv4Addr.Address); err != nil {
				t.Fatalf("s.AddAddress(%d, %d, %s): %s", nicID2, ipv4.ProtocolNumber, utils.Ipv4Addr.Address, err)
			}
			if err := s.AddAddress(nicID2, ipv6.ProtocolNumber, utils.Ipv6Addr.Address); err != nil {
				t.Fatalf("s.AddAddress(%d, %d, %s): %s", nicID2, ipv6.ProtocolNumber, utils.Ipv6Addr.Address, err)
			}

			if err := s.SetForwarding(ipv4.ProtocolNumber, true); err != nil {
				t.Fatalf("s.SetForwarding(%d, true): %s", ipv4.ProtocolNumber, err)
			}
			if err := s.SetForwarding(ipv6.ProtocolNumber, true); err != nil {
				t.Fatalf("s.SetForwarding(%d, true): %s", ipv6.ProtocolNumber, err)
			}

			s.SetRouteTable([]tcpip.Route{
				{
					Destination: header.IPv4EmptySubnet,
					NIC:         nicID2,
				},
				{
					Destination: header.IPv6EmptySubnet,
					NIC:         nicID2,
				},
			})

			test.rx(e1, test.srcAddr, test.dstAddr)

			p, ok := e2.Read()
			if ok != test.expectForward {
				t.Fatalf("got e2.Read() = (%#v, %t), want = (_, %t)", p, ok, test.expectForward)
			}

			if test.expectForward {
				test.checker(t, stack.PayloadSince(p.Pkt.NetworkHeader()))
			}
		})
	}
}

// TestForwardingWithoutFixingTransportChecksums tests that the stack will not
// fix transport layer checksums if they are unset or invalid.
func TestForwardingWithoutFixingTransportChecksums(t *testing.T) {
	const (
		nicID1 = 1
		nicID2 = 2

		ttl = 64
	)

	data := []byte{1, 2, 3, 4}

	rxUDPv4 := func(e *channel.Endpoint, checksum uint16) {
		utils.RxUDPv4(e, utils.RemoteIPv4Addr, utils.Ipv4Addr2.AddressWithPrefix.Address, ttl, data, func(u header.UDP) {
			u.SetChecksum(checksum)
		})
	}

	rxUDPv6 := func(e *channel.Endpoint, checksum uint16) {
		utils.RxUDPv6(e, utils.RemoteIPv6Addr, utils.Ipv6Addr2.AddressWithPrefix.Address, ttl, data, func(u header.UDP) {
			u.SetChecksum(checksum)
		})
	}

	v4Checker := func(t *testing.T, b []byte, checksum uint16) {
		checker.IPv4(t, b,
			checker.SrcAddr(utils.RemoteIPv4Addr),
			checker.DstAddr(utils.Ipv4Addr2.AddressWithPrefix.Address),
			checker.TTL(ttl-1),
			checker.UDP(
				checker.SrcPort(utils.RemotePort),
				checker.DstPort(utils.LocalPort),
				checker.Checksum(checksum),
				checker.Payload(data)))
	}

	v6Checker := func(t *testing.T, b []byte, checksum uint16) {
		checker.IPv6(t, b,
			checker.SrcAddr(utils.RemoteIPv6Addr),
			checker.DstAddr(utils.Ipv6Addr2.AddressWithPrefix.Address),
			checker.TTL(ttl-1),
			checker.UDP(
				checker.SrcPort(utils.RemotePort),
				checker.DstPort(utils.LocalPort),
				checker.Checksum(checksum),
				checker.Payload(data)))
	}

	tests := []struct {
		name    string
		rx      func(*channel.Endpoint, uint16)
		checker func(*testing.T, []byte, uint16)
	}{
		{
			name:    "IPv4 UDP",
			rx:      rxUDPv4,
			checker: v4Checker,
		},
		{
			name:    "IPv6 UDP",
			rx:      rxUDPv6,
			checker: v6Checker,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, c := range [...]uint16{0, 1, 2} {
				t.Run(fmt.Sprintf("Checksum=%d", c), func(t *testing.T) {
					s := stack.New(stack.Options{
						NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
						TransportProtocols: []stack.TransportProtocolFactory{udp.NewProtocol},
					})

					e1 := channel.New(0, header.IPv6MinimumMTU, "")
					if err := s.CreateNIC(nicID1, e1); err != nil {
						t.Fatalf("s.CreateNIC(%d, _): %s", nicID1, err)
					}

					e2 := channel.New(1, header.IPv6MinimumMTU, "")
					if err := s.CreateNIC(nicID2, e2); err != nil {
						t.Fatalf("s.CreateNIC(%d, _): %s", nicID2, err)
					}

					if err := s.AddAddress(nicID2, ipv4.ProtocolNumber, utils.Ipv4Addr.Address); err != nil {
						t.Fatalf("s.AddAddress(%d, %d, %s): %s", nicID2, ipv4.ProtocolNumber, utils.Ipv4Addr.Address, err)
					}
					if err := s.AddAddress(nicID2, ipv6.ProtocolNumber, utils.Ipv6Addr.Address); err != nil {
						t.Fatalf("s.AddAddress(%d, %d, %s): %s", nicID2, ipv6.ProtocolNumber, utils.Ipv6Addr.Address, err)
					}

					if err := s.SetForwarding(ipv4.ProtocolNumber, true); err != nil {
						t.Fatalf("s.SetForwarding(%d, true): %s", ipv4.ProtocolNumber, err)
					}
					if err := s.SetForwarding(ipv6.ProtocolNumber, true); err != nil {
						t.Fatalf("s.SetForwarding(%d, true): %s", ipv6.ProtocolNumber, err)
					}

					s.SetRouteTable([]tcpip.Route{
						{
							Destination: header.IPv4EmptySubnet,
							NIC:         nicID2,
						},
						{
							Destination: header.IPv6EmptySubnet,
							NIC:         nicID2,
						},
					})

					test.rx(e1, c)

					p, ok := e2.Read()
					if !ok {
						t.Fatal("expected packet to be forwarded")
					}

					test.checker(t, stack.PayloadSince(p.Pkt.NetworkHeader()), c)
				})
			}
		})
	}
}
