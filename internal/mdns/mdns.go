// Package mdns 是零第三方依赖的 mDNS/DNS-SD 发现:服务端广播(Announce),
// 客户端发现(Resolve),供 agent 实例互相发现后经 MCP Streamable HTTP 交互。
//
// 服务类型固定 "_agent-mcp._tcp";实例名 = -name 标志;TXT 携带 path=/mcp。
// 响应一律单播回源地址(mDNS 允许);应答器以 SO_REUSEADDR|SO_REUSEPORT 绑定
// 5353,与系统 avahi/systemd-resolved 共存。
package mdns

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// debug 是 MDNS_DEBUG=1 时的收包诊断输出(默认关闭)。
func debug() bool { return os.Getenv("MDNS_DEBUG") == "1" }

// soReusePort 在 Linux 上是 SO_REUSEPORT(15);内核未定义该常量的旧 Go 平台报错处理。
const soReusePort = 0x0f

// DefaultServiceType 是 agent 服务的 DNS-SD 服务类型。
const DefaultServiceType = "_agent-mcp._tcp"

const (
	mdnsAddr    = "224.0.0.251:5353"
	qtypeA      = 1
	qtypePTR    = 12
	qtypeTXT    = 16
	qtypeSRV    = 33
	classIN     = 1
	classCacheF = 0x8001 // 单播响应请求位(mDNS);此处仅用于识别
)

// Service 是一次发现的结果:足以拼出 http://IP:Port+Path。
type Service struct {
	Instance string // DNS-SD 实例名
	Host     string // SRV target(主机名)
	IP       string
	Port     int
	TXT      map[string]string
}

// Path 返回 TXT 中约定的路径;缺省 /mcp。
func (s Service) Path() string {
	if p, ok := s.TXT["path"]; ok && p != "" {
		return p
	}
	return "/mcp"
}

// URL 返回可直接交给 MCP HTTP 传输的地址。
func (s Service) URL() string {
	return fmt.Sprintf("http://%s:%d%s", s.IP, s.Port, s.Path())
}

// ---- DNS 报文(子集) ----

// dnsName 编码域名为 label 序列;不做压缩。
func dnsName(name string) []byte {
	var out []byte
	name = strings.TrimSuffix(name, ".")
	if name == "" {
		return append(out, 0)
	}
	for _, label := range strings.Split(name, ".") {
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}
	return append(out, 0)
}

// dnsReader 是带名字压缩指针解包的游标。
type dnsReader struct {
	msg []byte
	off int
	err error
}

func (r *dnsReader) u8() byte {
	if r.err != nil || r.off >= len(r.msg) {
		r.err = errors.New("mdns: truncated message")
		return 0
	}
	v := r.msg[r.off]
	r.off++
	return v
}

func (r *dnsReader) u16() uint16 {
	return uint16(r.u8())<<8 | uint16(r.u8())
}

// name 解读一个(可能压缩的)域名;返回值不含尾点。
func (r *dnsReader) name() string {
	if r.err != nil {
		return ""
	}
	var sb strings.Builder
	jumps := 0
	off := r.off
	for {
		if off >= len(r.msg) {
			r.err = errors.New("mdns: truncated name")
			return ""
		}
		b := r.msg[off]
		switch {
		case b == 0:
			off++
			if jumps == 0 {
				r.off = off
			}
			if sb.Len() == 0 {
				return "."
			}
			return sb.String()
		case b&0xC0 == 0xC0: // 压缩指针
			if off+1 >= len(r.msg) {
				r.err = errors.New("mdns: truncated pointer")
				return ""
			}
			ptr := int(b&0x3F)<<8 | int(r.msg[off+1])
			if jumps == 0 {
				r.off = off + 2 // 游标停在指针之后
			}
			jumps++
			if jumps > 16 || ptr >= off {
				r.err = errors.New("mdns: name pointer loop")
				return ""
			}
			off = ptr
		default:
			l := int(b)
			if off+1+l > len(r.msg) {
				r.err = errors.New("mdns: truncated label")
				return ""
			}
			if sb.Len() > 0 {
				sb.WriteByte('.')
			}
			sb.Write(r.msg[off+1 : off+1+l])
			off += 1 + l
		}
	}
}

// rr 是一条资源记录(答案/附加段共用)。
type rr struct {
	Name  string
	Type  uint16
	Class uint16
	Data  []byte
}

// dnsMessage 是解析后的 mDNS 报文(仅本包用到的字段)。
type dnsMessage struct {
	ID         uint16
	IsResponse bool
	Questions  []struct {
		Name  string
		Type  uint16
		Class uint16
	}
	Answers []rr
}

func parseDNS(msg []byte) (*dnsMessage, error) {
	if len(msg) < 12 {
		return nil, errors.New("mdns: short header")
	}
	m := &dnsMessage{
		ID:         binary.BigEndian.Uint16(msg[0:2]),
		IsResponse: msg[2]&0x80 != 0,
	}
	qd := int(binary.BigEndian.Uint16(msg[4:6]))
	an := int(binary.BigEndian.Uint16(msg[6:8]))
	r := &dnsReader{msg: msg, off: 12}
	for i := 0; i < qd; i++ {
		name := r.name()
		q := struct {
			Name  string
			Type  uint16
			Class uint16
		}{name, r.u16(), r.u16()}
		if r.err != nil {
			return nil, r.err
		}
		m.Questions = append(m.Questions, q)
	}
	for i := 0; i < an; i++ {
		name := r.name()
		typ, class := r.u16(), r.u16()
		r.u16() // ttl 高位(忽略)
		r.u16() // ttl 低位
		rdlen := int(r.u16())
		if r.err != nil || rdlen < 0 || r.off+rdlen > len(msg) {
			return nil, errors.New("mdns: bad rr")
		}
		m.Answers = append(m.Answers, rr{
			Name: name, Type: typ, Class: class,
			Data: msg[r.off : r.off+rdlen],
		})
		r.off += rdlen
	}
	return m, nil
}

// buildQuery 构造一次多问题查询。
func buildQuery(id uint16, questions []struct {
	Name string
	Type uint16
}) []byte {
	buf := make([]byte, 12)
	binary.BigEndian.PutUint16(buf[0:2], id)
	binary.BigEndian.PutUint16(buf[4:6], uint16(len(questions)))
	for _, q := range questions {
		buf = append(buf, dnsName(q.Name)...)
		var tail [4]byte
		binary.BigEndian.PutUint16(tail[0:2], q.Type)
		binary.BigEndian.PutUint16(tail[2:4], classIN)
		buf = append(buf, tail[:]...)
	}
	return buf
}

// buildResponse 构造一条应答(ID=0,QR=1)。
func buildResponse(answers []rr) []byte {
	buf := make([]byte, 12)
	buf[2] = 0x80 // QR=1
	binary.BigEndian.PutUint16(buf[6:8], uint16(len(answers)))
	for _, a := range answers {
		buf = append(buf, dnsName(a.Name)...)
		var head [10]byte
		binary.BigEndian.PutUint16(head[0:2], a.Type)
		binary.BigEndian.PutUint16(head[2:4], a.Class)
		binary.BigEndian.PutUint32(head[4:8], 120) // TTL
		binary.BigEndian.PutUint16(head[8:10], uint16(len(a.Data)))
		buf = append(buf, head[:]...)
		buf = append(buf, a.Data...)
	}
	return buf
}

// srvData 编码 SRV rdata。
func srvData(port int, target string) []byte {
	b := make([]byte, 6)
	binary.BigEndian.PutUint16(b[4:6], uint16(port))
	return append(b, dnsName(target)...)
}

// txtData 编码 TXT rdata(k=v;无 v 则裸 key)。
func txtData(txt map[string]string) []byte {
	var out []byte
	for _, k := range sortedKeys(txt) {
		s := k
		if v := txt[k]; v != "" {
			s = k + "=" + v
		}
		if len(s) > 255 {
			s = s[:255]
		}
		out = append(out, byte(len(s)))
		out = append(out, s...)
	}
	if len(out) == 0 {
		out = []byte{0}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

// ---- 应答器 ----

func listen5353() (*net.UDPConn, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var up []net.Interface
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp != 0 && ifi.Flags&net.FlagMulticast != 0 {
			up = append(up, ifi)
		}
	}
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var serr error
			err := c.Control(func(fd uintptr) {
				if e := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); e != nil {
					serr = e
				}
				if e := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, soReusePort, 1); e != nil && serr == nil {
					serr = e
				}
				// 加入组播组:ListenPacket 只绑端口,不生效成员身份,
				// 必须对每个支持组播的接口显式 IP_ADD_MEMBERSHIP(含 lo,
				// 否则同主机回环查询可能经隧道接口被丢弃,如 WireGuard)。
				for _, ifi := range ifaces {
					mreqn := &syscall.IPMreqn{
						Multiaddr: [4]byte{224, 0, 0, 251},
						Ifindex:   int32(ifi.Index),
					}
					if e := syscall.SetsockoptIPMreqn(int(fd), syscall.IPPROTO_IP, syscall.IP_ADD_MEMBERSHIP, mreqn); e != nil && serr == nil {
						serr = e
					}
				}
			})
			if err != nil {
				return err
			}
			return serr
		},
	}
	p, err := lc.ListenPacket(context.Background(), "udp4", ":5353")
	if err != nil {
		return nil, err
	}
	return p.(*net.UDPConn), nil
}

// outboundIP 取本机对外 IP(UDP connect 不发包)。
func outboundIP() (net.IP, error) {
	c, err := net.Dial("udp4", "8.8.8.8:53")
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP, nil
}

// Announce 启动 mDNS 应答器:开机主动播报两轮 + 响应 PTR/SRV/TXT/A 查询,
// 响应单播回源。返回 stop 函数;ctx 取消同样终止。
func Announce(ctx context.Context, instance string, port int, txt map[string]string) (stop func(), err error) {
	if instance == "" {
		return nil, errors.New("mdns: instance name required")
	}
	conn, err := listen5353()
	if err != nil {
		return nil, fmt.Errorf("mdns: bind 5353: %w", err)
	}
	ip, err := outboundIP()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("mdns: local ip: %w", err)
	}
	svc := DefaultServiceType + ".local"
	fqdn := instance + "." + svc
	host := fmt.Sprintf("%s.local", strings.ReplaceAll(strings.ToLower(instance), " ", "-"))

	answers := []rr{
		{Name: svc, Type: qtypePTR, Class: classIN, Data: dnsName(fqdn)},
		{Name: fqdn, Type: qtypeSRV, Class: classIN, Data: srvData(port, host)},
		{Name: fqdn, Type: qtypeTXT, Class: classIN, Data: txtData(txt)},
		{Name: host, Type: qtypeA, Class: classIN, Data: ip.To4()},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer conn.Close()
		// 主动播报两轮(间隔 1s),让局域网内的潜在调用方免等待。
		group, _ := net.ResolveUDPAddr("udp4", mdnsAddr)
		for round := 0; round < 2; round++ {
			conn.WriteToUDP(buildResponse(answers), group)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
		buf := make([]byte, 9000)
		for {
			n, remote, err := conn.ReadFromUDP(buf)
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					continue
				}
			}
			m, err := parseDNS(buf[:n])
			if debug() {
				log.Printf("mdns: rx %d bytes from %s parse_err=%v msg=%+v", n, remote, err, m)
			}
			if err != nil || m.IsResponse {
				continue
			}
			var reply []rr
			for _, q := range m.Questions {
				if q.Class&0x7F != classIN {
					continue
				}
				qn := strings.ToLower(strings.TrimSuffix(q.Name, "."))
				switch {
				case q.Type == qtypePTR && qn == strings.ToLower(svc):
					reply = append(reply, answers...)
				case q.Type == qtypeSRV && qn == strings.ToLower(fqdn):
					reply = append(reply, answers[1], answers[2], answers[3])
				case q.Type == qtypeTXT && qn == strings.ToLower(fqdn):
					reply = append(reply, answers[2])
				case q.Type == qtypeA && qn == strings.ToLower(host):
					reply = append(reply, answers[3])
				}
			}
			if len(reply) > 0 {
				conn.WriteToUDP(buildResponse(reply), remote)
			}
		}
	}()
	return func() {
		conn.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}, nil
}

// Resolve 在 wait 窗口内查询局域网,返回发现的全部服务;
// instance 非空时只保留同名词(不区分大小写)。
func Resolve(ctx context.Context, wait time.Duration, instance string) ([]Service, error) {
	if wait <= 0 {
		wait = 2 * time.Second
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	svc := DefaultServiceType + ".local"
	group, _ := net.ResolveUDPAddr("udp4", mdnsAddr)
	// 实例名未知前先查 PTR;SRV/TXT/A 直接问已知实例名(拿到答案前无效果,无害)。
	fqdn := "*." + svc
	if instance != "" {
		fqdn = instance + "." + svc
	}
	q := []struct {
		Name string
		Type uint16
	}{
		{Name: svc, Type: qtypePTR},
		{Name: fqdn, Type: qtypeSRV},
		{Name: fqdn, Type: qtypeTXT},
		{Name: fqdn, Type: qtypeA},
	}
	if _, err := conn.WriteToUDP(buildQuery(uint16(rand.Intn(0x10000)), q), group); err != nil {
		return nil, err
	}

	var mu sync.Mutex
	services := map[string]*Service{}
	deadline := time.Now().Add(wait)
	buf := make([]byte, 9000)
	for {
		conn.SetReadDeadline(deadline)
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			break // 超时
		}
		m, err := parseDNS(buf[:n])
		if debug() {
			log.Printf("mdns: resolve rx %d bytes parse_err=%v answers=%d", n, err, len(m.Answers))
		}
		if err != nil {
			continue
		}
		mu.Lock()
		// 两遍处理:SRV 先建立 host→instance 映射,A 记录按主机名归属实例。
		hosts := map[string]string{} // host(去 .local)→ instance
		for _, a := range m.Answers {
			name := strings.ToLower(strings.TrimSuffix(a.Name, "."))
			if a.Type != qtypeSRV {
				continue
			}
			suffix := strings.ToLower(svc)
			if !strings.HasSuffix(name, suffix) {
				continue
			}
			inst := strings.Trim(strings.TrimSuffix(name, suffix), ".")
			if inst == "" || (instance != "" && !strings.EqualFold(inst, instance)) {
				continue
			}
			s := serviceLocked(services, inst)
			s.Port = int(binary.BigEndian.Uint16(a.Data[4:6]))
			r := &dnsReader{msg: a.Data, off: 6}
			host := strings.Trim(r.name(), ".")
			s.Host = strings.TrimSuffix(host, ".local")
			hosts[strings.ToLower(host)] = inst
		}
		for _, a := range m.Answers {
			name := strings.ToLower(strings.TrimSuffix(a.Name, "."))
			switch a.Type {
			case qtypePTR:
				suffix := strings.ToLower(svc)
				if !strings.HasSuffix(name, suffix) {
					continue
				}
				inst := strings.Trim(strings.TrimSuffix(name, suffix), ".")
				if inst == "" || (instance != "" && !strings.EqualFold(inst, instance)) {
					continue
				}
				serviceLocked(services, inst)
			case qtypeA:
				inst, ok := hosts[name]
				if !ok {
					continue
				}
				if len(a.Data) == 4 {
					serviceLocked(services, inst).IP = net.IP(a.Data).String()
				}
			case qtypeTXT:
				suffix := strings.ToLower(svc)
				if !strings.HasSuffix(name, suffix) {
					continue
				}
				inst := strings.Trim(strings.TrimSuffix(name, suffix), ".")
				if inst == "" || (instance != "" && !strings.EqualFold(inst, instance)) {
					continue
				}
				s := serviceLocked(services, inst)
				for i := 0; i < len(a.Data); {
					l := int(a.Data[i])
					i++
					if l == 0 || i+l > len(a.Data) {
						break
					}
					kv := string(a.Data[i : i+l])
					i += l
					if k, v, found := strings.Cut(kv, "="); found {
						s.TXT[k] = v
					} else {
						s.TXT[kv] = ""
					}
				}
			}
		}
		mu.Unlock()
		if time.Now().After(deadline) {
			break
		}
	}
	// 补一轮:已有 PTR 但缺 SRV/A 的实例,定向补问。
	mu.Lock()
	var incomplete []string
	for name, s := range services {
		if s.IP == "" || s.Port == 0 {
			incomplete = append(incomplete, name)
		}
	}
	mu.Unlock()
	if len(incomplete) > 0 && time.Now().Before(deadline) {
		for _, name := range incomplete {
			f := name + "." + svc
			q2 := []struct {
				Name string
				Type uint16
			}{
				{Name: f, Type: qtypeSRV},
				{Name: f, Type: qtypeTXT},
				{Name: f, Type: qtypeA},
			}
			conn.WriteToUDP(buildQuery(uint16(rand.Intn(0x10000)), q2), group)
		}
		remaining := time.Until(deadline)
		if remaining > 0 {
			conn.SetReadDeadline(deadline)
			for i := 0; i < len(incomplete)*16; i++ {
				buf := make([]byte, 9000)
				if _, _, err := conn.ReadFromUDP(buf); err != nil {
					break
				}
				m, err := parseDNS(buf)
				if err != nil {
					continue
				}
				mu.Lock()
				applyAnswers(services, m, instance)
				mu.Unlock()
			}
		}
	}

	mu.Lock()
	defer mu.Unlock()
	out := make([]Service, 0, len(services))
	for _, s := range services {
		if s.IP != "" && s.Port != 0 {
			out = append(out, *s)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("mdns: no %s instance %q found within %s",
			DefaultServiceType, instance, wait)
	}
	return out, nil
}

// serviceLocked 取(或建)实例条目;调用方须持 mu。
func serviceLocked(services map[string]*Service, inst string) *Service {
	if s, ok := services[inst]; ok {
		return s
	}
	s := &Service{Instance: inst, TXT: map[string]string{}}
	services[inst] = s
	return s
}

// applyAnswers 把应答并入 services(供补问阶段复用)。
func applyAnswers(services map[string]*Service, m *dnsMessage, instance string) {
	for _, a := range m.Answers {
		name := strings.ToLower(strings.TrimSuffix(a.Name, "."))
		suffix := strings.ToLower(DefaultServiceType + ".local")
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		inst := strings.Trim(strings.TrimSuffix(name, suffix), ".")
		if inst == "" || (instance != "" && !strings.EqualFold(inst, instance)) {
			continue
		}
		s, ok := services[inst]
		if !ok {
			s = &Service{Instance: inst, TXT: map[string]string{}}
			services[inst] = s
		}
		switch a.Type {
		case qtypeSRV:
			s.Port = int(binary.BigEndian.Uint16(a.Data[4:6]))
		case qtypeA:
			if len(a.Data) == 4 {
				s.IP = net.IP(a.Data).String()
			}
		}
	}
}

// ResolveURL 便捷封装:发现并返回首个匹配实例的 URL(供 mdns: 协议接入)。
func ResolveURL(ctx context.Context, instance string, wait time.Duration) (string, error) {
	svcs, err := Resolve(ctx, wait, instance)
	if err != nil {
		return "", err
	}
	return svcs[0].URL(), nil
}
