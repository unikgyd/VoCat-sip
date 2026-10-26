//go:build linux && vocat_live_lab

package hostims

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
	"github.com/vishvananda/netlink"
	"vocat/internal/store"
)

// This opt-in hardware probe creates exactly one IMS bearer and reads its
// SIM/NAS/WDS preparation. It never sends SIP, voice calls or SMS. Run only in
// an operator-controlled window with the application line explicitly disabled.
// Full wire evidence stays in a private directory outside the source tree.
func TestLiveOwnedCellularPreparation(t *testing.T) {
	path := os.Getenv("VOCAT_HOSTIMS_LIVE_TEST_CONFIG")
	if path == "" {
		t.Skip("explicit private live-lab configuration required")
	}
	var cfg struct {
		AllowBearerConnect bool   `json:"allow_bearer_connect"`
		Database           string `json:"database"`
		DeviceID           string `json:"device_id"`
		Equipment          string `json:"equipment"`
		Physical           string `json:"physical"`
		ICCID              string `json:"iccid"`
		IMSI               string `json:"imsi"`
		Output             string `json:"output"`
		ProfileID          uint32 `json:"profile_id"`
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("private live configuration unavailable")
	}
	if json.Unmarshal(raw, &cfg) != nil || !cfg.AllowBearerConnect || cfg.Output == "" {
		t.Fatal("explicit live connection authorization required")
	}
	if !filepath.IsAbs(cfg.Output) || os.Mkdir(cfg.Output, 0700) != nil {
		t.Fatal("a new private evidence directory is required")
	}
	file, err := os.OpenFile(filepath.Join(cfg.Output, "wds-nas-private.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	database, err := store.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal("live database unavailable")
	}
	defer database.Close()
	device, err := database.Device(ctx, cfg.DeviceID)
	if err != nil || device.Telephony == nil || device.Telephony.AutoConnectCellular || device.Telephony.CellularMode != store.IMSModeHost || device.ModemIMEI != cfg.Equipment || device.USBPath != cfg.Physical || device.Telephony.ICCID != cfg.ICCID {
		t.Fatal("the exact application Host IMS line must be disabled")
	}
	connection, err := dbus.ConnectSystemBus()
	if err != nil {
		t.Fatal("system bus unavailable")
	}
	defer connection.Close()
	backend, err := NewModemManagerBackend(connection)
	if err != nil {
		t.Fatal(err)
	}
	journal := SQLiteClaimJournal{Database: database}
	pool, err := NewBearerPool(backend, journal, func(_ context.Context, s Selection) error {
		if s.SubscriberICCID() != cfg.ICCID || s.SubscriberIMSI() != cfg.IMSI {
			return ErrDenied
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	selection, err := pool.SelectTarget(ctx, ModemTarget{Equipment: cfg.Equipment, Physical: cfg.Physical, QMIControl: device.ControlDevice, ICCID: cfg.ICCID, IMSI: cfg.IMSI})
	if err != nil {
		t.Fatal("original modem/SIM selection failed")
	}
	if existing, err := journal.Read(ctx, selection.key); err != nil || len(existing) != 0 {
		t.Fatal("existing ownership claim prevents live probe")
	}
	lease, err := pool.Acquire(ctx, selection, BearerOptions{APN: "ims", IPFamily: 4, ProfileID: cfg.ProfileID, Timeout: time.Minute})
	if err != nil {
		t.Fatal("live bearer failed", PreparationFailureCode(err))
	}
	defer func() {
		cleanup, finish := context.WithTimeout(context.Background(), 30*time.Second)
		defer finish()
		if err := lease.Close(cleanup); err != nil {
			t.Error("owned live bearer cleanup did not acknowledge")
		}
	}()
	deps := runtimeOpenerDependencies{qmi: linuxOpenerDependencies(), topology: func(lease *BearerLease, pin endpointGuard) (wdsTopology, error) {
		control, ok := pin.(*qmiEndpointPin)
		if !ok {
			return nil, ErrQMIEndpoint
		}
		return pinWDSTopology(lease, control)
	}}
	deps.qmi.dial = func(ctx context.Context) (proxyChannel, error) {
		proxy, err := dialQMIProxy(ctx)
		if err != nil {
			return nil, err
		}
		return &liveTraceProxy{proxyQMI: proxy, file: file}, nil
	}
	nas, err := newLinuxNASServingCellSource(lease, deps)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := nas.Observe(ctx)
	if err != nil {
		t.Fatal("live owned NAS/WDS preparation failed", PreparationFailureCode(err))
	}
	if _, err := proof.RuntimeProof(); err != nil {
		t.Fatal("live runtime proof failed", PreparationFailureCode(err))
	}
	if link, err := netlink.LinkByName(lease.configuration.Interface); err == nil {
		a := link.Attrs()
		t.Logf("owned link kind=%s index=%d parent=%d master=%d flags=%s mtu=%d", link.Type(), a.Index, a.ParentIndex, a.MasterIndex, a.Flags, a.MTU)
	}
	if kernel, err := openLinuxNetworkKernel(lease, false); err != nil {
		t.Log("kernel preflight", PreparationFailureCode(err))
	} else {
		kernel.close()
	}
	sim, err := newLinuxSIMProvider(lease, deps.qmi)
	if err != nil {
		t.Fatal("SIM provider")
	}
	access, prepareErr := prepareAccessWith(ctx, lease, accessDependencies{sim: sim, observe: func(ctx context.Context) (accessSample, error) {
		proof, err := nas.Observe(ctx)
		if err != nil {
			return accessSample{}, err
		}
		runtime, err := proof.RuntimeProof()
		if err != nil {
			return accessSample{}, err
		}
		start, end := proof.QueryWindow()
		return accessSample{runtime: runtime, cell: proof.Sample(), start: start, end: end}, nil
	}, backend: &kernelPreparedAccess{lease: lease}})
	if access != nil {
		defer func() {
			cleanup, finish := context.WithTimeout(context.Background(), 30*time.Second)
			defer finish()
			if err := access.Close(cleanup); err != nil {
				t.Error("live access cleanup did not acknowledge")
			}
		}()
	}
	if prepareErr != nil {
		t.Fatal("live full access preparation failed", PreparationFailureCode(prepareErr))
	}
	for i := 0; i < 3; i++ {
		if err := access.Verify(ctx, access.CardIdentity().Subscriber()); err != nil {
			t.Fatal("live repeated verification failed", PreparationFailureCode(err))
		}
	}
	t.Log("real IMS bearer and bounded owned NAS/WDS observation succeeded; no SIP/call/SMS sent")
}

type liveTraceProxy struct {
	*proxyQMI
	file *os.File
	mu   sync.Mutex
}

func (p *liveTraceProxy) SendRequest(ctx context.Context, service, cid uint8, message uint16, input []qmi.TLV) (*qmi.Packet, error) {
	nasBefore, wdsBefore := p.nasRevision(), p.wdsRevision()
	packet, err := p.proxyQMI.SendRequest(ctx, service, cid, message, input)
	if service == qmi.ServiceUIM {
		p.mu.Lock()
		defer p.mu.Unlock()
		var code []byte
		var sizes []int
		if packet != nil {
			for _, tlv := range packet.TLVs {
				sizes = append(sizes, int(tlv.Type), len(tlv.Value))
				if tlv.Type == 2 {
					code = append([]byte(nil), tlv.Value...)
				}
			}
		}
		_ = json.NewEncoder(p.file).Encode(struct {
			Service uint8
			Message uint16
			Result  []byte
			Sizes   []int
			Failed  bool
		}{service, message, code, sizes, err != nil})
	} else if service == qmi.ServiceWDS || service == qmi.ServiceNAS {
		p.mu.Lock()
		defer p.mu.Unlock()
		_ = json.NewEncoder(p.file).Encode(struct {
			Service                                  uint8
			CID                                      uint8
			Message                                  uint16
			Input                                    []qmi.TLV
			Packet                                   *qmi.Packet
			Failed                                   bool
			NASBefore, NASAfter, WDSBefore, WDSAfter uint64
		}{service, cid, message, input, packet, err != nil, nasBefore, p.nasRevision(), wdsBefore, p.wdsRevision()})
	}
	return packet, err
}
