package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/megaease/easegateway/pkg/common"
	"github.com/megaease/easegateway/pkg/logger"
	"github.com/megaease/easegateway/pkg/option"

	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/clientv3/concurrency"
	"go.etcd.io/etcd/embed"
	"gopkg.in/yaml.v2"
)

const (
	// HeartbeatInterval is the interval for heartbeat.
	HeartbeatInterval = 5 * time.Second

	defragNormalInterval = 1 * time.Hour
	defragFailedInterval = 1 * time.Minute

	// waitServerTimeout is the timeout for waiting server to start.
	waitServerTimeout = 10 * time.Second

	// client config
	autoSyncInterval     = 1 * time.Minute
	dialTimeout          = 10 * time.Second
	dialKeepAliveTime    = 1 * time.Minute
	dialKeepAliveTimeout = 1 * time.Minute

	// lease config
	leaseTTL = clientv3.MaxLeaseTTL // 9000000000Second=285Year
)

type (
	// MemberStatus is the member status.
	MemberStatus struct {
		Options option.Options `yaml:"options"`

		// RFC3339 format
		LastHeartbeatTime string `yaml:"lastHeartbeatTime"`

		LastDefragTime string `yaml:"lastDefragTime,omitempty"`

		// Etcd is non-nil only it is a writer.
		Etcd *EtcdStatus `yaml:"etcd,omitempty"`
	}

	// EtcdStatus is the etcd status,
	// and extracts fields from server.Server.SelfStats.
	EtcdStatus struct {
		ID        string `yaml:"id"`
		StartTime string `yaml:"startTime"`
		State     string `yaml:"state"`
	}

	// etcdStats aims to extract fields from server.Server.SelfStats.
	etcdStats struct {
		ID        string    `json:"id"`
		State     string    `json:"state"`
		StartTime time.Time `json:"startTime"`
	}
)

func strTolease(s string) (clientv3.LeaseID, error) {
	lease, err := strconv.ParseInt(s, 16, 64)
	if err != nil {
		return 0, err
	}
	return clientv3.LeaseID(lease), nil
}

func newEtcdStats(buff []byte) (*etcdStats, error) {
	stats := etcdStats{}
	err := json.Unmarshal(buff, &stats)
	if err != nil {
		return nil, err
	}

	return &stats, nil
}

func (s *etcdStats) toEtcdStatus() *EtcdStatus {
	return &EtcdStatus{
		ID:        s.ID,
		State:     strings.TrimPrefix(s.State, "State"),
		StartTime: s.StartTime.Format(time.RFC3339),
	}
}

type cluster struct {
	opt            *option.Options
	requestTimeout time.Duration

	layout *Layout

	members *members

	server       *embed.Etcd
	client       *clientv3.Client
	lease        *clientv3.LeaseID
	session      *concurrency.Session
	serverMutex  sync.RWMutex
	clientMutex  sync.RWMutex
	leaseMutex   sync.RWMutex
	sessionMutex sync.RWMutex

	done chan struct{}
}

// New creates a cluster asynchronously,
// return non-nil err only if reaching hard limit.
func New(opt *option.Options) (Cluster, error) {
	// defensive programming
	requestTimeout, err := time.ParseDuration(opt.ClusterRequestTimeout)
	if err != nil {
		return nil, fmt.Errorf("invalid cluster request timeout: %v", err)
	}

	members, err := newMembers(opt)
	if err != nil {
		return nil, fmt.Errorf("new members failed: %v", err)
	}

	c := &cluster{
		opt:            opt,
		requestTimeout: requestTimeout,
		members:        members,
		done:           make(chan struct{}),
	}

	c.initLayout()

	go c.run()

	return c, nil
}

// requestContext returns context with request timeout,
// please use it immediately in case of incorrect timeout.
func (c *cluster) requestContext() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), c.requestTimeout)
	go func() {
		time.Sleep(c.requestTimeout)
		cancel()
	}()
	return ctx
}

// longRequestContext takes 3 times longer than requestContext.
func (c *cluster) longRequestContext() context.Context {
	requestTimeout := 3 * c.requestTimeout
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	go func() {
		time.Sleep(requestTimeout)
		cancel()
	}()
	return ctx
}

func (c *cluster) run() {

	logger.Infof("starting etcd cluster ...")
	for i := 0; ; i++ {
		err := c.getReady()
		if err != nil {
			logger.Errorf("failed to start cluster, retry count: %d, err:  %v", i, err)
			if i > 3 {
				logger.Errorf("[%s] failed to start etcd server too many times, contact the admin to check if other members online. \n" +
					"start other members if they're not online.\n" +
					"if other members are online, try to purge this node, clean and re-join it back.")
			}
			time.Sleep(HeartbeatInterval)
			continue
		}

		break
	}

	logger.Infof("cluster is ready")

	// FIXME: @miaojun Please care this routine in graceful update.
	if c.opt.ClusterRole == "writer" {
		go c.defrag()
	}

	c.heartbeat()
}

func (c *cluster) getReady() error {
	if c.opt.ClusterRole == "reader" {
		_, err := c.getClient()
		if err != nil {
			return err
		}
		err = c.initLease()
		if err != nil {
			return fmt.Errorf("init lease failed: %v", err)
		}
		return nil
	}

	if !c.opt.ForceNewCluster && c.members.knownMembersLen() > 1 {
		client, _ := c.getClient()
		if client != nil {
			err := c.addSelfToCluster()
			if err != nil {
				logger.Errorf("add self to cluster failed: %v", err)
			}
		}
	}

	done, timeout, err := c.startServer()
	if err != nil {
		return fmt.Errorf("start server failed: %v", err)
	}

	select {
	case <-done:
		_, err = c.getClient()
		if err != nil {
			return err
		}
	case <-timeout:
		return fmt.Errorf("start server timeout")
	}

	err = c.initLease()
	if err != nil {
		return fmt.Errorf("init lease failed: %v", err)
	}

	return nil
}

func (c *cluster) addSelfToCluster() error {
	client, err := c.getClient()
	if err != nil {
		return err
	}

	respList, err := client.MemberList(c.requestContext())
	if err != nil {
		return err
	}

	self := c.members.self()

	found := false
	for _, member := range respList.Members {
		if self.Name == member.Name && self.ID == member.ID {
			found = true
			break
		} else if self.Name == member.Name && self.ID != member.ID {
			logger.Errorf("conflict id, local name: %s, local id: %d, etcd name: %s, etcd id: %d"+
				"contact the admin to purge this node, clean and re-join it back.",
				self.Name, self.ID, member.Name, member.ID)
			panic("conflict etcd ids")
		} else if self.ID == member.ID && self.Name != member.Name {
			logger.Errorf("conflict name, local name: %s, local id: %d, etcd name: %s, etcd id: %d"+
				"contact the admin to purge this node, clean and re-join it back.",
				self.Name, self.ID, member.Name, member.ID)
			panic("conflict etcd names")
		}
	}

	if ! found {
		respAdd, err := client.MemberAdd(c.requestContext(), []string{c.opt.ClusterPeerURL})
		if err != nil {
			return err
		}
		logger.Infof("add %s to member list", self.Name)
		c.members.updateClusterMembers(respAdd.Members)
	}

	return nil
}

func (c *cluster) removeAndBackupEtcdData() {
	if !common.IsDirEmpty(c.opt.AbsDataDir) {
		logger.Infof("backup and clean %s", c.opt.AbsDataDir)
		err := common.BackupAndCleanDir(c.opt.AbsDataDir)
		if err != nil {
			logger.Errorf("backup and clean %s failed: %v", c.opt.AbsDataDir, err)
		}
	}
}

func (c *cluster) getClient() (*clientv3.Client, error) {
	c.clientMutex.RLock()
	if c.client != nil {
		client := c.client
		c.clientMutex.RUnlock()
		return client, nil
	}
	c.clientMutex.RUnlock()

	c.clientMutex.Lock()
	defer c.clientMutex.Unlock()

	// DCL
	if c.client != nil {
		return c.client, nil
	}

	endpoints := c.members.knownPeerURLs()
	if c.opt.ForceNewCluster {
		endpoints = []string{c.members.self().PeerURL}
	}
	logger.Infof("client connect with endpoints: %v", endpoints)
	client, err := clientv3.New(clientv3.Config{
		Endpoints:            endpoints,
		AutoSyncInterval:     autoSyncInterval,
		DialTimeout:          dialTimeout,
		DialKeepAliveTime:    dialKeepAliveTime,
		DialKeepAliveTimeout: dialKeepAliveTimeout,
		LogConfig:            logger.EtcdClientLoggerConfig(c.opt),
	})

	if err != nil {
		return nil, fmt.Errorf("create client failed: %v", err)
	}

	logger.Infof("client is ready")

	c.client = client

	return client, nil
}

func (c *cluster) closeClient() {
	c.clientMutex.Lock()
	defer c.clientMutex.Unlock()

	if c.client == nil {
		return
	}

	err := c.client.Close()
	if err != nil {
		logger.Errorf("close client failed: %v", err)
	}

	c.client = nil
}

func (c *cluster) getLease() (clientv3.LeaseID, error) {
	c.leaseMutex.RLock()
	defer c.leaseMutex.RUnlock()
	if c.lease == nil {
		return 0, fmt.Errorf("lease is not ready")
	}
	return *c.lease, nil
}

func (c *cluster) initLease() error {
	_, err := c.getLease()
	if err == nil {
		return nil
	}

	leaseStr, err := c.Get(c.Layout().Lease())
	if err != nil {
		return err
	}

	if leaseStr != nil {
		lease, err := strTolease(*leaseStr)
		if err != nil {
			logger.Errorf("BUG: parse lease %s failed: %v", *leaseStr, err)
			return err
		}

		c.leaseMutex.Lock()
		c.lease = &lease
		logger.Infof("lease is ready")
		c.leaseMutex.Unlock()

		return nil
	}

	client, err := c.getClient()
	if err != nil {
		return err
	}

	respGrant, err := client.Lease.Grant(c.requestContext(), leaseTTL)
	if err != nil {
		return err
	}
	lease := respGrant.ID

	// NOTE: In case of deadlock with calling PutUnderLease below.
	c.leaseMutex.Lock()
	c.lease = &lease
	logger.Infof("lease is ready")
	c.leaseMutex.Unlock()

	err = c.PutUnderLease(c.Layout().Lease(), fmt.Sprintf("%x", lease))
	if err != nil {
		return fmt.Errorf("put lease to %s failed: %v",
			c.Layout().Lease(), err)
	}

	return nil
}

func (c *cluster) getSession() (*concurrency.Session, error) {
	c.sessionMutex.RLock()
	if c.session != nil {
		session := c.session
		c.sessionMutex.RUnlock()
		return session, nil
	}
	c.sessionMutex.RUnlock()

	c.sessionMutex.Lock()
	defer c.sessionMutex.Unlock()

	// DCL
	if c.session != nil {
		return c.session, nil
	}

	client, err := c.getClient()
	if err != nil {
		return nil, err
	}

	lease, err := c.getLease()
	if err != nil {
		return nil, err
	}

	session, err := concurrency.NewSession(client,
		concurrency.WithLease(lease))
	if err != nil {
		return nil, fmt.Errorf("create session failed: %v", err)
	}

	logger.Infof("session is ready")

	return session, nil
}

func (c *cluster) closeSession() {
	c.sessionMutex.Lock()
	defer c.sessionMutex.Unlock()

	if c.session == nil {
		return
	}

	err := c.session.Close()
	if err != nil {
		logger.Errorf("close session failed: %v", err)
	}

	c.session = nil
}

func (c *cluster) getServer() (*embed.Etcd, error) {
	c.serverMutex.RLock()
	defer c.serverMutex.RUnlock()
	if c.server == nil {
		return nil, fmt.Errorf("server is not ready")
	}
	return c.server, nil
}

func closeEtcdServer(s *embed.Etcd) {
	select {
	case <-s.Server.ReadyNotify():
		s.Close()
		<-s.Server.StopNotify()
	default:
		s.Server.HardStop()
		logger.Infof("hard stop server")
	}
	for _, client := range s.Clients {
		client.Close()
	}
	for _, peer := range s.Peers {
		peer.Close()
	}
}

func (c *cluster) startServer() (done, timeout chan struct{}, err error) {
	c.serverMutex.Lock()
	defer c.serverMutex.Unlock()

	done, timeout = make(chan struct{}), make(chan struct{})
	if c.server != nil {
		close(done)
		return done, timeout, nil
	}

	etcdConfig, err := c.prepareEtcdConfig()
	if err != nil {
		return nil, nil, err
	}

	server, err := embed.StartEtcd(etcdConfig)
	if err != nil {
		return nil, nil, err
	}

	monitorServer := func(s *embed.Etcd) {
		select {
		case err := <-s.Err():
			logger.Errorf("%s serve faield: %v",
				c.server.Config().Name, err.Error())
			closeEtcdServer(s)
		case <-c.done:
			return
		}
	}

	go func() {
		select {
		case <-c.done:
			return
		case <-server.Server.ReadyNotify():
			c.server = server
			go monitorServer(c.server)
			logger.Infof("server is ready")
			close(done)
		case <-time.After(waitServerTimeout):
			closeEtcdServer(server)
			close(timeout)
		}
	}()

	return done, timeout, nil

}

func (c *cluster) closeServer() {
	c.serverMutex.Lock()
	defer c.serverMutex.Unlock()

	if c.server == nil {
		return
	}

	closeEtcdServer(c.server)
	c.server = nil
}

func (c *cluster) CloseServer(wg *sync.WaitGroup) {
	defer wg.Done()
	c.closeServer()
}

func (c *cluster) StartServer() (done, timeout chan struct{}, err error) {
	return c.startServer()
}

func (c *cluster) heartbeat() {
	for {
		select {
		case <-time.After(HeartbeatInterval):
			err := c.syncStatus()
			if err != nil {
				logger.Errorf("sync status failed: %v", err)
			}
			err = c.updateMembers()
			if err != nil {
				logger.Errorf("update members failed: %v", err)
			}
		case <-c.done:
			return
		}
	}
}

func (c *cluster) defrag() {
	defragInterval := defragNormalInterval
	for {
		select {
		case <-time.After(defragInterval):
			client, err := c.getClient()
			if err != nil {
				defragInterval = defragFailedInterval
				logger.Errorf("defrag failed: get client failed: %v", err)
			}

			// NOTICE: It need longer time than normal ones.
			_, err = client.Defragment(c.longRequestContext(), c.opt.ClusterPeerURL)
			if err != nil {
				defragInterval = defragFailedInterval
				logger.Errorf("defrag failed: %v", err)
				continue
			}

			logger.Infof("defrag successfully")
			defragInterval = defragNormalInterval
		case <-c.done:
			return
		}
	}
}

func (c *cluster) syncStatus() error {
	status := MemberStatus{
		Options: *c.opt,
	}

	if c.opt.ClusterRole == "writer" {
		server, err := c.getServer()
		if err != nil {
			return err
		}

		buff := server.Server.SelfStats()
		stats, err := newEtcdStats(buff)
		if err != nil {
			return err
		}
		status.Etcd = stats.toEtcdStatus()
	}

	status.LastHeartbeatTime = time.Now().Format(time.RFC3339)

	buff, err := yaml.Marshal(status)
	if err != nil {
		return err
	}

	err = c.PutUnderLease(c.Layout().StatusMemberKey(), string(buff))
	if err != nil {
		return fmt.Errorf("put status failed: %v", err)
	}
	return nil
}

func (c *cluster) updateMembers() error {
	client, err := c.getClient()
	if err != nil {
		return err
	}

	resp, err := client.MemberList(c.requestContext())
	if err != nil {
		return err
	}

	c.members.updateClusterMembers(resp.Members)

	return nil
}

func (c *cluster) PurgeMember(memberName string) error {
	client, err := c.getClient()
	if err != nil {
		return err
	}

	// remove etcd member if there is it.
	respList, err := client.MemberList(c.requestContext())
	if err != nil {
		return err
	}
	var id *uint64
	for _, member := range respList.Members {
		if member.Name == memberName {
			id = &member.ID
		}
	}
	if id != nil {
		_, err = client.MemberRemove(c.requestContext(), *id)
		if err != nil {
			return err
		}
	}

	// remove all stuff under the lease of the member.
	leaseKey := c.Layout().OtherLease(memberName)
	leaseStr, err := c.Get(leaseKey)
	if err != nil {
		return err
	}
	if leaseStr == nil {
		return fmt.Errorf("%s not found", leaseKey)
	}
	lease, err := strTolease(*leaseStr)
	if err != nil {
		return err
	}

	_, err = client.Lease.Revoke(c.requestContext(), lease)
	if err != nil {
		return err
	}

	return nil
}

func (c *cluster) Close(wg *sync.WaitGroup) {
	defer wg.Done()

	close(c.done)

	c.closeSession()
	c.closeClient()
	c.closeServer()
}
