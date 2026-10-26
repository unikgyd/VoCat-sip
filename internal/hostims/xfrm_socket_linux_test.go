//go:build linux

package hostims

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

func socketPolicyFixture() SocketSecurityPolicy {
	return SocketSecurityPolicy{Local: netip.MustParseAddr("192.0.2.2"), Remote: netip.MustParseAddr("192.0.2.1"),
		LocalPort: 62000, Protocol: unix.IPPROTO_UDP, Role: SocketSecurityClient, ReqID: 0x6001}
}

func TestSocketSecurityPolicyEncoding(t *testing.T) {
	for _, version := range []string{"ipv4", "ipv6"} {
		for _, role := range []SocketSecurityRole{SocketSecurityClient, SocketSecurityServer} {
			for _, protocol := range []uint8{unix.IPPROTO_UDP, unix.IPPROTO_TCP} {
				p := socketPolicyFixture()
				p.Role, p.Protocol = role, protocol
				family := unix.AF_INET
				if version == "ipv6" {
					p.Local, p.Remote = netip.MustParseAddr("2001:db8::2"), netip.MustParseAddr("2001:db8::1")
					family = unix.AF_INET6
				}
				in, out, gotFamily, err := socketSecurityPolicies(p)
				if err != nil || gotFamily != family {
					t.Fatalf("%s role=%d proto=%d: %v family=%d", version, role, protocol, err, gotFamily)
				}
				for dir, buf := range [][]byte{in, out} {
					if len(buf) != nl.SizeofXfrmUserpolicyInfo+nl.SizeofXfrmUserTmpl {
						t.Fatalf("socket policy length = %d", len(buf))
					}
					info := nl.DeserializeXfrmUserpolicyInfo(buf)
					tmpl := nl.DeserializeXfrmUserTmpl(buf[nl.SizeofXfrmUserpolicyInfo:])
					source, dest := p.Remote, p.Local
					if dir == 1 {
						source, dest = p.Local, p.Remote
					}
					if info.Dir != uint8(dir) || info.Action != 0 || info.Flags != 0 ||
						info.Sel != (nl.XfrmSelector{Family: uint16(family)}) || tmpl.Family != uint16(family) || tmpl.Reqid != p.ReqID ||
						tmpl.Mode != 0 || tmpl.Optional != 0 || tmpl.XfrmId.Proto != unix.IPPROTO_ESP || tmpl.XfrmId.Spi != 0 {
						t.Fatalf("unexpected required transport ESP encoding (%s dir=%d)", version, dir)
					}
					if !net.IP(tmpl.Saddr[:len(source.AsSlice())]).Equal(net.IP(source.AsSlice())) ||
						!net.IP(tmpl.XfrmId.Daddr[:len(dest.AsSlice())]).Equal(net.IP(dest.AsSlice())) {
						t.Fatal("template endpoints do not match")
					}
					if info.Lft.HardByteLimit != ^uint64(0) || info.Lft.SoftByteLimit != ^uint64(0) ||
						info.Lft.HardPacketLimit != ^uint64(0) || info.Lft.SoftPacketLimit != ^uint64(0) {
						t.Fatal("socket policy can expire into a different policy")
					}
				}
			}
		}
	}
}

func TestSocketSecurityPolicyRejectsInvalid(t *testing.T) {
	cases := map[string]func(*SocketSecurityPolicy){
		"invalid local":  func(p *SocketSecurityPolicy) { p.Local = netip.Addr{} },
		"invalid remote": func(p *SocketSecurityPolicy) { p.Remote = netip.Addr{} },
		"unspecified":    func(p *SocketSecurityPolicy) { p.Local = netip.MustParseAddr("0.0.0.0") },
		"multicast":      func(p *SocketSecurityPolicy) { p.Remote = netip.MustParseAddr("224.0.0.1") },
		"same endpoint":  func(p *SocketSecurityPolicy) { p.Remote = p.Local },
		"mixed family":   func(p *SocketSecurityPolicy) { p.Remote = netip.MustParseAddr("2001:db8::1") },
		"mapped family":  func(p *SocketSecurityPolicy) { p.Local = netip.MustParseAddr("::ffff:192.0.2.2") },
		"zone":           func(p *SocketSecurityPolicy) { p.Local = netip.MustParseAddr("fe80::2%test") },
		"zero port":      func(p *SocketSecurityPolicy) { p.LocalPort = 0 },
		"reserved port":  func(p *SocketSecurityPolicy) { p.LocalPort = 1024 },
		"clear SIP port": func(p *SocketSecurityPolicy) { p.LocalPort = 5060 },
		"TLS SIP port":   func(p *SocketSecurityPolicy) { p.LocalPort = 5061 },
		"linklocal":      func(p *SocketSecurityPolicy) { p.Remote = netip.MustParseAddr("169.254.1.1") },
		"zero reqid":     func(p *SocketSecurityPolicy) { p.ReqID = 0 },
		"zero protocol":  func(p *SocketSecurityPolicy) { p.Protocol = 0 },
		"esp protocol":   func(p *SocketSecurityPolicy) { p.Protocol = unix.IPPROTO_ESP },
		"unknown role":   func(p *SocketSecurityPolicy) { p.Role = 3 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			p := socketPolicyFixture()
			change(&p)
			if _, _, _, err := socketSecurityPolicies(p); !errors.Is(err, ErrSocketSecurityPolicy) {
				t.Fatal("invalid policy accepted")
			}
		})
	}
	if RequireSocketESP(nil, socketPolicyFixture()) != ErrSocketSecurityPolicy {
		t.Fatal("nil raw socket accepted")
	}
}

func TestSocketSecurityPolicySocketShape(t *testing.T) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.IPPROTO_UDP)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	p := socketPolicyFixture()
	if !socketSecurityMatches(fd, p, unix.AF_INET) {
		t.Fatal("unbound UDP control callback rejected")
	}
	wrong := p
	wrong.Protocol = unix.IPPROTO_TCP
	if socketSecurityMatches(fd, wrong, unix.AF_INET) || socketSecurityMatches(fd, p, unix.AF_INET6) {
		t.Fatal("wrong family or protocol accepted")
	}
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	addr, err := unix.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	p.Local, p.LocalPort = netip.MustParseAddr("127.0.0.1"), uint16(addr.(*unix.SockaddrInet4).Port)
	if !socketSecurityMatches(fd, p, unix.AF_INET) {
		t.Fatal("exact bound source rejected")
	}
	p.LocalPort++
	if socketSecurityMatches(fd, p, unix.AF_INET) {
		t.Fatal("wrong bound port accepted")
	}
}

// This test requires both NEWNET and NEWNS, refuses any existing interface or
// XFRM object, and injects only TEST-NET/documentation-IPv6 frames on its veth.
// No modem, registrar, default route, real key or host service is involved.
func TestSocketSecurityPolicyNative(t *testing.T) {
	if os.Getenv("VOCAT_XFRM_SOCKET_TEST") != "1" {
		t.Skip("requires VOCAT_XFRM_SOCKET_TEST=1 in a fresh NET+MNT namespace")
	}
	for _, ns := range []string{"net", "mnt"} {
		self, err := os.Stat("/proc/self/ns/" + ns)
		init, other := os.Stat("/proc/1/ns/" + ns)
		if err != nil || other != nil || os.SameFile(self, init) {
			t.Fatalf("refusing non-private %s namespace", ns)
		}
	}
	links, err := net.Interfaces()
	if err != nil || len(links) != 1 || links[0].Name != "lo" {
		t.Fatal("native socket test requires only loopback")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("isolated setup %s: %v: %s", args[0], err, out)
		}
		return string(out)
	}
	if strings.TrimSpace(run("ip", "xfrm", "state", "list")) != "" || strings.TrimSpace(run("ip", "xfrm", "policy", "list")) != "" {
		t.Fatal("refusing existing XFRM objects")
	}
	run("ip", "link", "add", "vxsp0", "type", "veth", "peer", "name", "vxsp1")
	// The whole private namespace is destroyed on process exit. Deliberately do
	// not flush or delete an object in a possibly different namespace on error.
	run("ip", "link", "set", "vxsp0", "up")
	run("ip", "link", "set", "vxsp1", "up")
	run("ip", "addr", "add", "192.0.2.2/32", "dev", "vxsp0")
	run("ip", "-6", "addr", "add", "2001:db8:1::2/128", "dev", "vxsp0", "nodad")
	run("sysctl", "-w", "net.ipv4.conf.all.rp_filter=0")
	run("sysctl", "-w", "net.ipv4.conf.vxsp0.rp_filter=0")
	run("ip", "route", "add", "192.0.2.1/32", "dev", "vxsp0", "src", "192.0.2.2")
	run("ip", "-6", "route", "add", "2001:db8:1::1/128", "dev", "vxsp0", "src", "2001:db8:1::2")
	peer, err := net.InterfaceByName("vxsp1")
	if err != nil {
		t.Fatal(err)
	}
	run("ip", "neigh", "add", "192.0.2.1", "lladdr", peer.HardwareAddr.String(), "nud", "permanent", "dev", "vxsp0")
	run("ip", "-6", "neigh", "add", "2001:db8:1::1", "lladdr", peer.HardwareAddr.String(), "nud", "permanent", "dev", "vxsp0")
	var files []*os.File
	for _, variant := range []struct{ family, proto, port, reqid int }{
		{unix.AF_INET, unix.IPPROTO_UDP, 62000, 0x6001},
		{unix.AF_INET6, unix.IPPROTO_UDP, 62002, 0x6003},
		{unix.AF_INET, unix.IPPROTO_TCP, 62001, 0x6002},
	} {
		kind := unix.SOCK_DGRAM
		p := socketPolicyFixture()
		if variant.proto == unix.IPPROTO_TCP {
			kind, p.Role = unix.SOCK_STREAM, SocketSecurityServer
		}
		fd, err := unix.Socket(variant.family, kind|unix.SOCK_CLOEXEC, variant.proto)
		if err != nil {
			t.Fatal(err)
		}
		file := os.NewFile(uintptr(fd), "synthetic-protected-socket")
		t.Cleanup(func() { file.Close() })
		if err := unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, "vxsp0"); err != nil {
			t.Fatal(err)
		}
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_MARK, 0x7001); err != nil {
			t.Fatal(err)
		}
		var address unix.Sockaddr = &unix.SockaddrInet4{Addr: [4]byte{192, 0, 2, 2}, Port: variant.port}
		if variant.family == unix.AF_INET6 {
			p.Local, p.Remote = netip.MustParseAddr("2001:db8:1::2"), netip.MustParseAddr("2001:db8:1::1")
			address = &unix.SockaddrInet6{Addr: p.Local.As16(), Port: variant.port}
		}
		p.LocalPort, p.Protocol, p.ReqID = uint16(variant.port), uint8(variant.proto), uint32(variant.reqid)
		raw, err := file.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		prebind := variant.family == unix.AF_INET6 || kind == unix.SOCK_STREAM
		if prebind && RequireSocketESP(raw, p) != nil {
			t.Fatal("production helper failed in pre-bind Control lifecycle")
		}
		if err := unix.Bind(fd, address); err != nil {
			t.Fatal(err)
		}
		if !prebind && RequireSocketESP(raw, p) != nil {
			t.Fatal("production helper failed to install required IN/OUT socket policies")
		}
		if kind == unix.SOCK_STREAM {
			if err := unix.Listen(fd, 1); err != nil {
				t.Fatal(err)
			}
		}
		files = append(files, file)
	}
	child := exec.CommandContext(ctx, "python3", "-c", socketPolicyNativePython)
	child.ExtraFiles = files
	out, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("native required-policy packet tests: %v: %s", err, out)
	}
	t.Log(strings.TrimSpace(string(out)))
}

// The Go production helper above, not C/ctypes, configured inherited fds 3..5.
// This peer only supplies synthetic wire packets and verifies operational ESP.
const socketPolicyNativePython = `
import hashlib,hmac,json,select,socket,struct,subprocess,time
from cryptography.hazmat.primitives.ciphers import Cipher,algorithms,modes
def run(*args):
 p=subprocess.run(args,capture_output=True,text=True,timeout=3)
 if p.returncode: raise RuntimeError((args[:4],p.stderr))
 return p.stdout
local,remote='192.0.2.2','192.0.2.1'
local6,remote6='2001:db8:1::2','2001:db8:1::1'
key,ik=b'\x11'*16,b'\x22'*20
def addsa(src,dst,spi,reqid):
 args=['ip','xfrm','state','add','src',src,'dst',dst,'proto','esp','spi',hex(spi),'reqid',str(reqid),
 'mode','transport','replay-window','32','auth-trunc','hmac(sha1)','0x'+ik.hex(),'96','enc','cbc(aes)','0x'+key.hex()]
 if src in (local,local6):args+=['mark','0','mask','0xffffffff']
 run(*args)
for src,dst,reqid,spi in [(remote,local,0x6001,0x1001),(remote,local,0x6002,0x1002),(remote6,local6,0x6003,0x1003)]:
 addsa(src,dst,spi,reqid); addsa(dst,src,spi+0x1000,reqid)
assert not run('ip','xfrm','policy','list','nosock').strip(), 'global policy must remain empty'
mac=lambda dev: bytes.fromhex(json.loads(run('ip','-j','link','show',dev))[0]['address'].replace(':',''))
dest,source=mac('vxsp0'),mac('vxsp1')
wire=socket.socket(socket.AF_PACKET,socket.SOCK_RAW,socket.htons(3)); wire.bind(('vxsp1',3)); wire.setblocking(False)
u4,u6,listener=[socket.socket(fileno=fd) for fd in (3,4,5)]
for s in (u4,u6,listener): s.settimeout(.2)
def csum(b):
 if len(b)%2: b+=b'\0'
 n=sum(struct.unpack('!%dH'%(len(b)//2),b))
 while n>>16:n=(n&65535)+(n>>16)
 return (~n)&65535
def esp(body,proto,spi,seq):
 pad=(16-(len(body)+2)%16)%16; plain=body+bytes(range(1,pad+1))+bytes((pad,proto))
 iv=seq.to_bytes(16,'big'); e=Cipher(algorithms.AES(key),modes.CBC(iv)).encryptor()
 body=struct.pack('!II',spi,seq)+iv+e.update(plain)+e.finalize()
 return body+hmac.new(ik,body,hashlib.sha1).digest()[:12]
def unesp(body):
 assert hmac.compare_digest(hmac.new(ik,body[:-12],hashlib.sha1).digest()[:12],body[-12:]),'bad outgoing ICV'
 d=Cipher(algorithms.AES(key),modes.CBC(body[8:24])).decryptor(); b=d.update(body[24:-12])+d.finalize()
 return b[-1],b[:len(b)-2-b[-2]]
def inject(segment,proto,spi,seq,v6=False,protected=True):
 body=esp(segment,proto,spi,seq) if protected else segment; nh=50 if protected else proto
 if v6:
  ip=struct.pack('!IHBB16s16s',6<<28,len(body),nh,64,socket.inet_pton(10,remote6),socket.inet_pton(10,local6)); eth=b'\x86\xdd'
 else:
  ip=struct.pack('!BBHHHBBH4s4s',0x45,0,len(body)+20,seq,0,64,nh,0,socket.inet_aton(remote),socket.inet_aton(local))
  ip=ip[:10]+struct.pack('!H',csum(ip))+ip[12:];eth=b'\x08\x00'
 wire.send(dest+source+eth+ip+body)
def recvwire(proto,port,timeout=.3):
 end=time.monotonic()+timeout
 while time.monotonic()<end:
  if not select.select([wire],[],[],max(0,end-time.monotonic()))[0]:break
  frame=wire.recv(65535)
  if len(frame)<54:continue
  if frame[12:14]==b'\x08\x00' and frame[26:30]==socket.inet_aton(local):
   nh=frame[23]; body=frame[14+(frame[14]&15)*4:]
  elif frame[12:14]==b'\x86\xdd' and frame[22:38]==socket.inet_pton(10,local6):
   nh=frame[20]; body=frame[54:]
  else:continue
  protected=nh==50
  if protected:nh,body=unesp(body)
  if nh==proto and len(body)>=8 and struct.unpack('!H',body[:2])[0]==port:return protected,body
 return None
def udp(text,port,v6=False):
 b=struct.pack('!HHHH',61000,port,len(text)+8,0)+text
 if v6:
  pseudo=socket.inet_pton(10,remote6)+socket.inet_pton(10,local6)+struct.pack('!I3xB',len(b),17)
  b=b[:6]+struct.pack('!H',csum(pseudo+b))+b[8:]
 return b
def blocked(s):
 try: data=s.recv(2048)
 except socket.timeout:return
 raise AssertionError(('plaintext or wrong-reqid delivered',data))
result={}
for s,v6,port,spi in [(u4,False,62000,0x1001),(u6,True,62002,0x1003)]:
 inject(udp(b'plain-must-drop',port,v6),17,spi,1,v6,False);blocked(s)
 inject(udp(b'required-ESP-accepted',port,v6),17,spi,2,v6)
 assert s.recv(2048)==b'required-ESP-accepted'
 saved4,saved6=remote,remote6
 if v6:remote6='2001:db8:1::99'
 else:remote='192.0.2.99'
 inject(udp(b'other-peer-plaintext-must-drop',port,v6),17,spi,8,v6,False);blocked(s)
 remote,remote6=saved4,saved6
 target=(remote6 if v6 else remote,61099)
 s.sendto(b'outgoing-required-ESP',target)
 got=recvwire(17,port);assert got and got[0] and got[1][8:]==b'outgoing-required-ESP','UDP output not ESP'
 result['udp6' if v6 else 'udp4']='same/other peer plaintext blocked; ESP input+output verified; ephemeral remote port'
# A packet which decrypts successfully under a wrong reqid must still fail.
inject(udp(b'wrong-pair-must-drop',62000),17,0x1002,3);blocked(u4)
run('ip','xfrm','state','delete','src',local,'dst',remote,'proto','esp','spi','0x2001')
try:u4.sendto(b'no-SA-must-not-leak',(remote,61099))
except OSError:pass
assert recvwire(17,62000,.25) is None,'missing outgoing SA allowed plaintext'
result['outbound_missing_sa']='no plaintext emitted'
def tcp(seq,ack,flags,payload=b''):
 b=struct.pack('!HHIIHHHH',61001,62001,seq,ack,(5<<12)|flags,8192,0,0)+payload
 pseudo=socket.inet_aton(remote)+socket.inet_aton(local)+struct.pack('!BBH',0,6,len(b))
 return b[:16]+struct.pack('!H',csum(pseudo+b))+b[18:]
inject(tcp(1000,0,2),6,0x1002,4)
reply=recvwire(6,62001);assert reply and reply[0],'SYN-ACK must be protected'
sport,dport,serverseq,ack,flags=struct.unpack('!HHIIH',reply[1][:14]);assert flags&0x12==0x12
inject(tcp(1001,serverseq+1,16),6,0x1002,5)
accepted,_=listener.accept();accepted.settimeout(.2)
assert accepted.getsockopt(socket.SOL_SOCKET,socket.SO_BINDTODEVICE,32).rstrip(b'\0')==b'vxsp0'
assert accepted.getsockopt(socket.SOL_SOCKET,socket.SO_MARK)==0x7001
inject(tcp(1001,serverseq+1,24,b'plain-data-must-drop'),6,0x1002,6,protected=False);blocked(accepted)
inject(tcp(1001,serverseq+1,24,b'tcp-inherited-required-ESP'),6,0x1002,7)
assert accepted.recv(2048)==b'tcp-inherited-required-ESP'
accepted.sendall(b'tcp-protected-answer')
reply=recvwire(6,62001)
while reply and not reply[1][(reply[1][12]>>4)*4:]:reply=recvwire(6,62001)
assert reply and reply[0] and reply[1][(reply[1][12]>>4)*4:]==b'tcp-protected-answer'
result['tcp4']='accepted inherits device+mark+IN/OUT required ESP; plaintext blocked; ESP input+output verified'
assert not run('ip','xfrm','policy','list','nosock').strip(),'unexpected global policy'
print(json.dumps(result,sort_keys=True))
`
