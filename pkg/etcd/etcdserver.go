package etcd

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	"golang.org/x/net/context"
	protoetcd "kope.io/etcd-manager/pkg/apis/etcd"
	"kope.io/etcd-manager/pkg/backup"
	"kope.io/etcd-manager/pkg/contextutil"
	"kope.io/etcd-manager/pkg/privateapi"
)

const PreparedValidity = time.Minute

type EtcdServer struct {
	baseDir     string
	peerServer  *privateapi.Server
	nodeInfo    *protoetcd.EtcdNode
	clusterName string

	backupStore backup.Store

	mutex sync.Mutex

	state    *protoetcd.EtcdState
	prepared *preparedState
	process  *etcdProcess
}

type preparedState struct {
	validUntil   time.Time
	clusterToken string
}

func NewEtcdServer(baseDir string, clusterName string, nodeInfo *protoetcd.EtcdNode, peerServer *privateapi.Server) *EtcdServer {
	s := &EtcdServer{
		baseDir:     baseDir,
		clusterName: clusterName,
		peerServer:  peerServer,
		nodeInfo:    nodeInfo,
	}

	protoetcd.RegisterEtcdManagerServiceServer(peerServer.GrpcServer(), s)
	return s
}

var _ protoetcd.EtcdManagerServiceServer = &EtcdServer{}

func (s *EtcdServer) Run(ctx context.Context) {
	contextutil.Forever(ctx, time.Second*10, func() {
		err := s.runOnce()
		if err != nil {
			glog.Warningf("error running etcd: %v", err)
		}
	})
}

func readState(baseDir string) (*protoetcd.EtcdState, error) {
	p := filepath.Join(baseDir, "state")
	b, err := ioutil.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("error reading state file %q: %v", p, err)
	}

	state := &protoetcd.EtcdState{}
	if err := proto.Unmarshal(b, state); err != nil {
		// TODO: Have multiple state files?
		return nil, fmt.Errorf("error parsing state file: %v", err)
	}

	return state, nil
}

func writeState(baseDir string, state *protoetcd.EtcdState) error {
	p := filepath.Join(baseDir, "state")

	b, err := proto.Marshal(state)
	if err != nil {
		return fmt.Errorf("error marshaling state data: %v", err)
	}

	if err := ioutil.WriteFile(p, b, 0755); err != nil {
		return fmt.Errorf("error writing state file %q: %v", p, err)
	}
	return nil
}

func (s *EtcdServer) runOnce() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.state == nil {
		state, err := readState(s.baseDir)
		if err != nil {
			return err
		}

		if state != nil {
			s.state = state
		}
	}

	// Check that etcd process is still running
	if s.process != nil {
		exitError, exitState := s.process.ExitState()
		if exitError != nil || exitState != nil {
			glog.Warningf("etc process exited (error=%v, state=%v)", exitError, exitState)

			s.process = nil
		}
	}

	// Start etcd, if it is not running but should be
	if s.state != nil && s.process == nil {
		if err := s.startEtcdProcess(s.state); err != nil {
			return err
		}
	}

	return nil
}

// GetInfo gets info about the node
func (s *EtcdServer) GetInfo(context.Context, *protoetcd.GetInfoRequest) (*protoetcd.GetInfoResponse, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	response := &protoetcd.GetInfoResponse{}
	response.ClusterName = s.clusterName
	if s.state != nil && s.state.Cluster != nil {
		//pb := &protoetcd.EtcdCluster{}
		//*pb = *s.state.Cluster
		response.EtcdConfigured = true
		response.ClusterToken = s.state.Cluster.ClusterToken
		//response.ClusterConfiguration = pb
	}
	response.NodeConfiguration = s.nodeInfo

	return response, nil
}

// JoinCluster requests that the node join an existing cluster
func (s *EtcdServer) JoinCluster(ctx context.Context, request *protoetcd.JoinClusterRequest) (*protoetcd.JoinClusterResponse, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if request.ClusterName != s.clusterName {
		glog.Infof("request had incorrect ClusterName.  ClusterName=%q but request=%q", s.clusterName, request)
		return nil, fmt.Errorf("ClusterName mismatch")
	}

	// TODO: Validate (our) peer id?

	if !s.peerServer.IsLeader(request.LeadershipToken) {
		return nil, fmt.Errorf("LeadershipToken in request %q is not current leader", request.LeadershipToken)
	}

	if s.prepared != nil && time.Now().After(s.prepared.validUntil) {
		glog.Infof("preparation %q expired", s.prepared.clusterToken)
		s.prepared = nil
	}

	response := &protoetcd.JoinClusterResponse{}

	switch request.Phase {
	case protoetcd.Phase_PHASE_PREPARE:
		if s.process != nil {
			return nil, fmt.Errorf("etcd process already running")
		}

		if s.prepared != nil {
			return nil, fmt.Errorf("concurrent prepare in progress %q", s.prepared.clusterToken)
		}

		s.prepared = &preparedState{
			validUntil:   time.Now().Add(PreparedValidity),
			clusterToken: request.ClusterToken,
		}

	case protoetcd.Phase_PHASE_INITIAL_CLUSTER:
		if s.process != nil {
			return nil, fmt.Errorf("etcd process already running")
		}

		if s.prepared == nil {
			return nil, fmt.Errorf("not prepared")
		}
		if s.prepared.clusterToken != request.ClusterToken {
			return nil, fmt.Errorf("clusterToken %q does not match prepared %q", request.ClusterToken, s.prepared.clusterToken)
		}

		if s.state == nil {
			s.state = &protoetcd.EtcdState{}
		}
		s.state.NewCluster = true
		s.state.Cluster = &protoetcd.EtcdCluster{
			ClusterToken: request.ClusterToken,
			Nodes:        request.Nodes,
		}

		if err := writeState(s.baseDir, s.state); err != nil {
			return nil, err
		}

		if err := s.startEtcdProcess(s.state); err != nil {
			return nil, err
		}

		// TODO: Wait for etcd initialization before marking as existing?
		s.state.NewCluster = false
		if err := writeState(s.baseDir, s.state); err != nil {
			return nil, err
		}

	case protoetcd.Phase_PHASE_JOIN_EXISTING:
		if s.process != nil {
			return nil, fmt.Errorf("etcd process already running")
		}

		if s.prepared == nil {
			return nil, fmt.Errorf("not prepared")
		}
		if s.prepared.clusterToken != request.ClusterToken {
			return nil, fmt.Errorf("clusterToken %q does not match prepared %q", request.ClusterToken, s.prepared.clusterToken)
		}

		if s.state == nil {
			s.state = &protoetcd.EtcdState{}
		}
		s.state.NewCluster = false
		s.state.Cluster = &protoetcd.EtcdCluster{
			ClusterToken: request.ClusterToken,
			Nodes:        request.Nodes,
		}

		if err := writeState(s.baseDir, s.state); err != nil {
			return nil, err
		}

		if err := s.startEtcdProcess(s.state); err != nil {
			return nil, err
		}
		// TODO: Wait for join?

	default:
		return nil, fmt.Errorf("unknown status %s", request.Phase)
	}

	return response, nil
}

// DoBackup performs a backup to the backupstore
func (s *EtcdServer) DoBackup(ctx context.Context, request *protoetcd.DoBackupRequest) (*protoetcd.DoBackupResponse, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.clusterName != request.ClusterName {
		glog.Infof("request had incorrect ClusterName.  ClusterName=%q but request=%q", s.clusterName, request)
		return nil, fmt.Errorf("ClusterName mismatch")
	}

	if !s.peerServer.IsLeader(request.LeadershipToken) {
		return nil, fmt.Errorf("LeadershipToken in request %q is not current leader", request.LeadershipToken)
	}

	if s.process == nil {
		return nil, fmt.Errorf("etcd not running")
	}

	if request.Storage == "" {
		return nil, fmt.Errorf("Storage is required")
	}
	if request.State == nil {
		return nil, fmt.Errorf("State is required")
	}
	backupStore, err := backup.NewStore(request.Storage)
	if err != nil {
		return nil, err
	}

	response, err := s.process.DoBackup(backupStore, request.State)
	if err != nil {
		return nil, err
	}
	return response, nil
}

func (s *EtcdServer) startEtcdProcess(state *protoetcd.EtcdState) error {
	dataDir := filepath.Join(s.baseDir, "data", state.Cluster.ClusterToken)
	glog.Infof("starting etcd with datadir %s", dataDir)

	// TODO: Validate this during the PREPARE phase
	var meNode *protoetcd.EtcdNode
	for _, node := range state.Cluster.Nodes {
		if stringSlicesEqual(node.ClientUrls, s.nodeInfo.ClientUrls) {
			if meNode != nil {
				glog.Infof("Nodes: %v", state.Cluster.Nodes)
				return fmt.Errorf("multiple nodes matching local client urls %s included in cluster", node.ClientUrls)
			}
			meNode = node
		}
	}
	if meNode == nil {
		glog.Infof("self node: %v", s.nodeInfo)
		glog.Infof("cluster: %v", state.Cluster.Nodes)
		return fmt.Errorf("self node was not included in cluster")
	}

	// TODO: Force choice to localhost?
	clientURL := s.nodeInfo.ClientUrls[0]

	p := &etcdProcess{
		CreateNewCluster: false,
		BinDir:           "/opt/etcd-v2.2.1-linux-amd64",
		DataDir:          dataDir,
		ClientURL:        clientURL,
		Cluster: &protoetcd.EtcdCluster{
			ClusterToken: state.Cluster.ClusterToken,
			Me:           meNode,
			Nodes:        state.Cluster.Nodes,
		},
	}

	if state.NewCluster {
		p.CreateNewCluster = true
	}

	if err := p.Start(); err != nil {
		return fmt.Errorf("error starting etcd: %v", err)
	}

	s.process = p

	return nil
}

// StopEtcdProcess terminates etcd if it is running; primarily used for testing
func (s *EtcdServer) StopEtcdProcess() (bool, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.process == nil {
		return false, nil
	}

	glog.Infof("killing etcd with datadir %s", s.process.DataDir)
	err := s.process.Stop()
	if err != nil {
		return true, fmt.Errorf("error killing etcd: %v", err)
	}
	s.process = nil
	return true, nil
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i, s := range a {
		if b[i] != s {
			return false
		}
	}
	return true
}
