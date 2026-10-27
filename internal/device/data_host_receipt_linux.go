//go:build linux

package device

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"

	"github.com/vishvananda/netlink"
)

func dataReceiptDigest(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func dataReceiptHash(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == 32
}
func dataReceiptFamily(family int) int {
	if family == 6 {
		return netlink.FAMILY_V6
	}
	return netlink.FAMILY_V4
}

func dataReceiptRule(name string, family int, value netlink.Rule) bool {
	mark, table, priority := exportProxyRouteIdentity(name)
	if value.Mask != nil && *value.Mask != ^uint32(0) {
		return false
	}
	if value.Protocol != 0 && value.Protocol != 3 && value.Protocol != 4 {
		return false
	}
	want := netlink.NewRule()
	want.Family, want.Mark, want.Table, want.Priority = dataReceiptFamily(family), mark, table, priority
	want.Mask, want.Protocol, want.Type = value.Mask, value.Protocol, 1 // FR_ACT_TO_TBL
	return reflect.DeepEqual(*want, value)
}

// ValidateDataHostReceipt validates metadata only. A valid record is not itself
// permission to mutate a live interface or to delete an unrecognised object.
func ValidateDataHostReceipt(h DataHostReceipt) error {
	if h.Version != 1 || h.Interface == "" || len(h.Interface) > 15 || filepath.Base(h.Interface) != h.Interface || h.Interface == "." || h.Interface == ".." || h.InterfaceIndex <= 0 || len(h.Families) == 0 || len(h.Families) > 2 {
		return ErrDataCleanupPending
	}
	for _, c := range h.Interface {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return ErrDataCleanupPending
		}
	}
	seen := map[int]bool{}
	for _, f := range h.Families {
		if f != 4 && f != 6 || seen[f] {
			return ErrDataCleanupPending
		}
		seen[f] = true
	}
	if h.Ready && (len(h.Rules) != len(seen) || len(h.RouteHashes) != len(seen) || h.DNS == nil || len(h.Addresses) != len(seen)) {
		return ErrDataCleanupPending
	}
	for f, raw := range h.Rules {
		var rule netlink.Rule
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if !seen[f] || decoder.Decode(&rule) != nil || !dataReceiptRule(h.Interface, f, rule) {
			return ErrDataCleanupPending
		}
		canonical, _ := json.Marshal(rule)
		if !bytes.Equal(canonical, raw) {
			return ErrDataCleanupPending
		}
	}
	for f, hash := range h.RouteHashes {
		if !seen[f] || !dataReceiptHash(hash) {
			return ErrDataCleanupPending
		}
	}
	if h.DNS != nil {
		d := h.DNS
		if d.Device == 0 || d.Inode == 0 || d.DirectoryDevice == 0 || d.DirectoryInode == 0 || d.Mode != 0644 || !dataReceiptHash(d.SHA256) {
			return ErrDataCleanupPending
		}
	}
	addresses := map[int]bool{}
	for _, address := range h.Addresses {
		ip, _, err := net.ParseCIDR(address)
		if err != nil || !ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast() {
			return ErrDataCleanupPending
		}
		family := 6
		if ip.To4() != nil {
			family = 4
		}
		if !seen[family] || addresses[family] {
			return ErrDataCleanupPending
		}
		addresses[family] = true
	}
	return nil
}

func dataReceiptRoutes(name string, family, index int) (string, error) {
	_, table, _ := exportProxyRouteIdentity(name)
	routes, err := netlink.RouteListFiltered(dataReceiptFamily(family), &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return "", err
	}
	canonical := make([]string, 0, len(routes))
	for _, route := range routes {
		if route.LinkIndex != index {
			return "", ErrDataCleanupPending
		}
		route.Flags = 0 // kernel carrier/link flags are not ownership selectors
		data, err := json.Marshal(route)
		if err != nil {
			return "", err
		}
		canonical = append(canonical, string(data))
	}
	sort.Strings(canonical)
	return dataReceiptDigest([]byte(strings.Join(canonical, "\n"))), nil
}

func dataReceiptDNS(name string) (*DataHostFile, error) {
	path := exportProxyDNSPath(name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	dir, dok := parent.Sys().(*syscall.Stat_t)
	if !ok || !dok || !info.Mode().IsRegular() || info.Mode().Perm() != 0644 || st.Nlink != 1 || st.Uid != uint32(os.Geteuid()) || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 || parent.Mode().Perm()&0022 != 0 || dir.Uid != st.Uid || info.Size() <= 0 || info.Size() > 4096 {
		return nil, ErrDataCleanupPending
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return nil, ErrDataCleanupPending
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || int64(len(data)) != info.Size() || after.Size() != info.Size() || after.ModTime() != info.ModTime() {
		return nil, ErrDataCleanupPending
	}
	return &DataHostFile{Device: uint64(st.Dev), Inode: st.Ino, DirectoryDevice: uint64(dir.Dev), DirectoryInode: dir.Ino, UID: st.Uid, Mode: uint32(info.Mode().Perm()), SHA256: dataReceiptDigest(data)}, nil
}

func dataReceiptAddresses(name string) ([]string, error) {
	info, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	values, err := info.Addrs()
	if err != nil {
		return nil, err
	}
	var result []string
	for _, value := range values {
		ip, _, err := net.ParseCIDR(value.String())
		if err == nil && ip.IsGlobalUnicast() && !ip.IsLinkLocalUnicast() {
			result = append(result, value.String())
		}
	}
	sort.Strings(result)
	return result, nil
}

func cloneDataHostReceipt(h DataHostReceipt) DataHostReceipt {
	raw, _ := json.Marshal(h)
	var copy DataHostReceipt
	_ = json.Unmarshal(raw, &copy)
	return copy
}

func validateCurrentDataHost(h DataHostReceipt) error {
	if err := ValidateDataHostReceipt(h); err != nil {
		return err
	}
	info, err := net.InterfaceByName(h.Interface)
	if err != nil || info.Index != h.InterfaceIndex {
		return ErrDataCleanupPending
	}
	addresses, err := dataReceiptAddresses(h.Interface)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(addresses, h.Addresses) {
		return ErrDataCleanupPending
	}
	for _, family := range h.Families {
		rules, err := dataReceiptRules(h.Interface, family)
		if err != nil {
			return err
		}
		if len(h.Rules) == 0 {
			if len(rules) != 0 {
				return ErrDataCleanupPending
			}
		} else {
			if len(rules) != 1 {
				return ErrDataCleanupPending
			}
			raw, _ := json.Marshal(rules[0])
			if !bytes.Equal(raw, h.Rules[family]) {
				return ErrDataCleanupPending
			}
		}
		hash, err := dataReceiptRoutes(h.Interface, family, h.InterfaceIndex)
		if err != nil {
			return err
		}
		wanted := h.RouteHashes[family]
		if wanted == "" {
			wanted = dataReceiptDigest(nil)
		}
		if hash != wanted {
			return ErrDataCleanupPending
		}
	}
	dns, err := dataReceiptDNS(h.Interface)
	if h.DNS == nil {
		if !errors.Is(err, os.ErrNotExist) {
			return ErrDataCleanupPending
		}
	} else if err != nil || !reflect.DeepEqual(dns, h.DNS) {
		return ErrDataCleanupPending
	}
	return nil
}

func beginDataHostReceipt(ctx context.Context, owner *ownedNetwork, ipVersion string) error {
	if owner == nil {
		return nil
	}
	claim, durable := owner.claim.(DataHostClaim)
	if !durable {
		return nil
	}
	h := DataHostReceipt{Version: 1, Interface: owner.candidate.NetworkInterface, InterfaceIndex: owner.interfaceID, Families: []int{4}}
	if ipVersion == "IPV6" {
		h.Families = []int{6}
	} else if ipVersion == "IPV4V6" {
		h.Families = []int{4, 6}
	}
	if owner.hostReceipt != nil {
		h = cloneDataHostReceipt(*owner.hostReceipt)
	}
	if err := validateCurrentDataHost(h); err != nil {
		return err
	}
	h.Ready = false
	if err := claim.SaveHost(ctx, h); err != nil {
		return err
	}
	owner.hostReceipt = &h
	return nil
}

func commitDataHostReceipt(ctx context.Context, owner *ownedNetwork) error {
	if owner == nil {
		return nil
	}
	claim, durable := owner.claim.(DataHostClaim)
	if !durable {
		return nil
	}
	if owner.hostReceipt == nil || !owner.handleKnown {
		return ErrDataCleanupPending
	}
	h := cloneDataHostReceipt(*owner.hostReceipt)
	h.Rules = map[int]json.RawMessage{}
	h.RouteHashes = map[int]string{}
	for _, family := range h.Families {
		rules, err := dataReceiptRules(h.Interface, family)
		if err != nil || len(rules) != 1 || !dataReceiptRule(h.Interface, family, rules[0]) {
			return ErrDataCleanupPending
		}
		h.Rules[family], _ = json.Marshal(rules[0])
		hash, err := dataReceiptRoutes(h.Interface, family, h.InterfaceIndex)
		if err != nil {
			return err
		}
		h.RouteHashes[family] = hash
	}
	var err error
	h.Addresses, err = dataReceiptAddresses(h.Interface)
	if err != nil {
		return err
	}
	h.DNS, err = dataReceiptDNS(h.Interface)
	if err != nil {
		return err
	}
	h.Ready = true
	if err := ValidateDataHostReceipt(h); err != nil {
		return err
	}
	if err := claim.SaveHost(ctx, h); err != nil {
		return err
	}
	owner.hostReceipt = &h
	return nil
}

// CleanupOrphanDataHost is called under the recovered equipment lock, after
// owner death and original USB generation loss have independently been proved.
// Kernel deletion of the original netdev must have removed all of its routes
// and addresses. No replacement interface is brought down or reconfigured.
func CleanupOrphanDataHost(ctx context.Context, h DataHostReceipt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateDataHostReceipt(h); err != nil || !h.Ready {
		return ErrDataCleanupPending
	}
	if _, err := netlink.LinkByIndex(h.InterfaceIndex); err == nil {
		return ErrDataCleanupPending
	} else {
		var missing netlink.LinkNotFoundError
		if !errors.As(err, &missing) {
			return err
		}
	}
	var remove []netlink.Rule
	for _, family := range h.Families {
		hash, err := dataReceiptRoutes(h.Interface, family, h.InterfaceIndex)
		if err != nil || hash != dataReceiptDigest(nil) {
			return ErrDataCleanupPending
		}
		rules, err := dataReceiptRules(h.Interface, family)
		if err != nil || len(rules) > 1 {
			return ErrDataCleanupPending
		}
		if len(rules) == 1 {
			raw, _ := json.Marshal(rules[0])
			if !bytes.Equal(raw, h.Rules[family]) {
				return ErrDataCleanupPending
			}
			remove = append(remove, rules[0])
		}
	}
	dns, err := dataReceiptDNS(h.Interface)
	if err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && !reflect.DeepEqual(dns, h.DNS) {
		return ErrDataCleanupPending
	}
	for _, rule := range remove {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := netlink.RuleDel(&rule); err != nil && !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	if dns != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		again, err := dataReceiptDNS(h.Interface)
		if err != nil || !reflect.DeepEqual(again, h.DNS) {
			return ErrDataCleanupPending
		}
		if err := os.Remove(exportProxyDNSPath(h.Interface)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, family := range h.Families {
		rules, err := dataReceiptRules(h.Interface, family)
		if err != nil || len(rules) != 0 {
			return ErrDataCleanupPending
		}
		hash, err := dataReceiptRoutes(h.Interface, family, h.InterfaceIndex)
		if err != nil || hash != dataReceiptDigest(nil) {
			return ErrDataCleanupPending
		}
	}
	if _, err := os.Lstat(exportProxyDNSPath(h.Interface)); !errors.Is(err, os.ErrNotExist) {
		return ErrDataCleanupPending
	}
	if _, err := netlink.LinkByIndex(h.InterfaceIndex); err == nil {
		return ErrDataCleanupPending
	} else {
		var missing netlink.LinkNotFoundError
		if !errors.As(err, &missing) {
			return err
		}
	}
	return nil
}
