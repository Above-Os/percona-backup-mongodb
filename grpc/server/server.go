package server

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/percona/mongodb-backup/internal/notify"
	pb "github.com/percona/mongodb-backup/proto/messages"
	"github.com/pkg/errors"
	"github.com/prometheus/common/log"
	"github.com/sirupsen/logrus"
)

const (
	EVENT_BACKUP_FINISH = iota
	EVENT_OPLOG_FINISH
	EVENT_RESTORE_FINISH
)

type MessagesServer struct {
	stopChan chan struct{}
	lock     *sync.Mutex
	clients  map[string]*Client
	// Current backup status
	replicasRunningBackup map[string]bool // Key is ReplicasetUUID
	lastOplogTs           int64           // Timestamp in Unix format
	backupRunning         bool
	oplogBackupRunning    bool
	restoreRunning        bool
	err                   error
	//
	workDir               string
	lastBackupMetadata    *BackupMetadata
	clientDisconnetedChan chan string
	dbBackupFinishChan    chan interface{}
	oplogBackupFinishChan chan interface{}
	restoreFinishChan     chan interface{}
	logger                *logrus.Logger
}

func NewMessagesServer(workDir string, logger *logrus.Logger) *MessagesServer {
	if logger == nil {
		logger = logrus.New()
		logger.SetLevel(logrus.StandardLogger().Level)
		logger.Out = logrus.StandardLogger().Out
	}

	bfc := notify.Start(EVENT_BACKUP_FINISH)
	ofc := notify.Start(EVENT_OPLOG_FINISH)
	rbf := notify.Start(EVENT_RESTORE_FINISH)

	messagesServer := &MessagesServer{
		lock:                  &sync.Mutex{},
		clients:               make(map[string]*Client),
		clientDisconnetedChan: make(chan string),
		stopChan:              make(chan struct{}),
		dbBackupFinishChan:    bfc,
		oplogBackupFinishChan: ofc,
		restoreFinishChan:     rbf,
		replicasRunningBackup: make(map[string]bool),
		workDir:               workDir,
		logger:                logger,
	}

	go messagesServer.handleClientDisconnection()

	return messagesServer
}

func (s *MessagesServer) BackupSourceByReplicaset() (map[string]*Client, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	sources := make(map[string]*Client)
	for _, client := range s.clients {
		if _, ok := sources[client.ReplicasetName]; !ok {
			if client.NodeType == pb.NodeType_MONGOS {
				continue
			}
			backupSource, err := client.GetBackupSource()
			if err != nil {
				s.logger.Errorf("Cannot get backup source for client %s: %s", client.NodeName, err)
			}
			fmt.Printf("Received backup source from client %s: %s\n", client.NodeName, backupSource)
			if err != nil {
				return nil, fmt.Errorf("Cannot get best client for replicaset %q: %s", client.ReplicasetName, err)
			}
			bestClient := s.getClientByNodeName(backupSource)
			if bestClient == nil {
				bestClient = client
			}
			sources[client.ReplicasetName] = bestClient
		}
	}
	return sources, nil
}

func (s *MessagesServer) Clients() map[string]Client {
	s.lock.Lock()
	defer s.lock.Unlock()

	c := make(map[string]Client)
	for id, client := range s.clients {
		c[id] = *client
	}
	return c
}

func (s *MessagesServer) ClientsByReplicaset() map[string][]Client {
	replicas := make(map[string][]Client)
	for _, client := range s.clients {
		if _, ok := replicas[client.ReplicasetName]; !ok {
			replicas[client.ReplicasetName] = make([]Client, 0)
		}
		replicas[client.ReplicasetName] = append(replicas[client.ReplicasetName], *client)

	}
	return replicas
}

// IsShardedSystem returns if a system is sharded.
// It check if the Node Type is:
// - Mongos
// - Config Server
// - Shard Server
// or if the ClusterID is not empty because in a sharded system, the cluster id
// is never empty.
func (s *MessagesServer) IsShardedSystem() bool {
	for _, client := range s.clients {
		if client.NodeType == pb.NodeType_MONGOS ||
			client.NodeType == pb.NodeType_MONGOD_CONFIGSVR ||
			client.NodeType == pb.NodeType_MONGOD_SHARDSVR ||
			client.ClusterID != "" {
			return true
		}
	}
	return false
}

func (s MessagesServer) LastBackupMetadata() *BackupMetadata {
	return s.lastBackupMetadata
}

func (s *MessagesServer) ReplicasetsRunningDBBackup() map[string]*Client {
	replicasets := make(map[string]*Client)

	for _, client := range s.clients {
		if client.isDBBackupRunning() {
			replicasets[client.ReplicasetName] = client
		}
	}
	return replicasets
}

func (s *MessagesServer) ReplicasetsRunningOplogBackup() map[string]*Client {
	replicasets := make(map[string]*Client)

	// use sync.Map?
	for _, client := range s.clients {
		if client.isOplogTailerRunning() {
			replicasets[client.ReplicasetName] = client
		}
	}

	return replicasets
}

func (s *MessagesServer) ReplicasetsRunningRestore() map[string]*Client {
	replicasets := make(map[string]*Client)

	// use sync.Map?
	for _, client := range s.clients {
		if client.isRestoreRunning() {
			replicasets[client.ReplicasetName] = client
		}
	}

	return replicasets
}

// RestoreBackUp will run a restore on each client, using the provided backup metadata to choose the source for each
// replicaset.
func (s *MessagesServer) RestoreBackUp(bm *pb.BackupMetadata, SkipUsersAndRoles bool) error {
	clients, err := s.BackupSourceByReplicaset()
	if err != nil {
		return errors.Wrapf(err, "Cannot start backup restore. Cannot find backup source for replicas")
	}

	if s.isBackupRunning() {
		return fmt.Errorf("Cannot start a restore while a backup is running")
	}
	if s.isRestoreRunning() {
		return fmt.Errorf("Cannot start a restore while another restore is still running")
	}

	s.reset()
	s.setRestoreRunning(true)

	for replName, client := range clients {
		s.logger.Printf("Starting backup for replicaset %q on client %s %s %s", replName, client.ID, client.NodeName, client.NodeType)
		s.replicasRunningBackup[replName] = true
		for bmReplName, metadata := range bm.Replicasets {
			if bmReplName == replName {
				client.setRestoreRunning(true)
				client.restoreBackup(&pb.RestoreBackup{
					BackupType:        bm.BackupType,
					SourceType:        bm.DestinationType,
					SourceBucket:      bm.DestinationDir,
					DBSourceName:      metadata.DBBackupName,
					OplogSourceName:   metadata.OplogBackupName,
					CompressionType:   bm.CompressionType,
					Cypher:            bm.Cypher,
					SkipUsersAndRoles: SkipUsersAndRoles,
				})
			}
		}
	}

	return nil
}

func getFileExtension(compressionType pb.CompressionType, cypher pb.Cypher) string {
	ext := ""

	switch cypher {
	case pb.Cypher_NO_CYPHER:
	}

	switch compressionType {
	case pb.CompressionType_GZIP:
		ext = ext + ".gzip"
	}

	return ext
}

// TODO Create an API StartBackup message instead of using pb.StartBackup
// For example, we don't need DBBackupName & OplogBackupName and having them,
// here if we use pb.StartBackup message, leads to confusions
func (s *MessagesServer) StartBackup(opts *pb.StartBackup) error {
	if s.isBackupRunning() {
		return fmt.Errorf("Cannot start a backup while another backup is still running")
	}
	if s.isRestoreRunning() {
		return fmt.Errorf("Cannot start a backup while a restore is still running")
	}

	ext := getFileExtension(pb.CompressionType(opts.CompressionType), pb.Cypher(opts.Cypher))

	s.lastBackupMetadata = NewBackupMetadata(opts)

	clients, err := s.BackupSourceByReplicaset()
	if err != nil {
		return errors.Wrapf(err, "Cannot start backup. Cannot find backup source for replicas")
	}

	s.reset()
	s.setBackupRunning(true)
	s.setOplogBackupRunning(true)

	for replName, client := range clients {
		s.logger.Printf("Starting backup for replicaset %q on client %s %s %s", replName, client.ID, client.NodeName, client.NodeType)
		s.replicasRunningBackup[replName] = true

		dbBackupName := fmt.Sprintf("%s_%s.dump%s", opts.NamePrefix, client.ReplicasetName, ext)
		oplogBackupName := fmt.Sprintf("%s_%s.oplog%s", opts.NamePrefix, client.ReplicasetName, ext)

		s.lastBackupMetadata.AddReplicaset(client.ReplicasetName, client.ReplicasetUUID, dbBackupName, oplogBackupName)

		client.startBackup(&pb.StartBackup{
			BackupType:      opts.GetBackupType(),
			DestinationType: opts.GetDestinationType(),
			DBBackupName:    dbBackupName,
			OplogBackupName: oplogBackupName,
			DestinationDir:  opts.GetDestinationDir(),
			CompressionType: opts.GetCompressionType(),
			Cypher:          opts.GetCypher(),
			OplogStartTime:  opts.GetOplogStartTime(),
		})
	}

	return nil
}

// StartBalancer restarts the balancer if this is a sharded system
func (s *MessagesServer) StartBalancer() error {
	s.lock.Lock()
	defer s.lock.Unlock()
	for _, client := range s.clients {
		if client.NodeType == pb.NodeType_MONGOS {
			return client.startBalancer()
		}
	}
	// This is not a sharded system. There is nothing to do.
	return nil
}

func (s *MessagesServer) Stop() {
	close(s.stopChan)
}

// StopBalancer stops the balancer if this is a sharded system
func (s *MessagesServer) StopBalancer() error {
	s.lock.Lock()
	defer s.lock.Unlock()
	for _, client := range s.clients {
		if client.NodeType == pb.NodeType_MONGOS {
			return client.stopBalancer()
		}
	}
	// This is not a sharded system. There is nothing to do.
	return nil
}

// StopOplogTail calls ever agent StopOplogTail(ts) method using the last oplog timestamp reported by the clients
// when they call DBBackupFinished after mongodump finish on each client. That way s.lastOplogTs has the last
// timestamp of the slowest backup
func (s *MessagesServer) StopOplogTail() error {
	if !s.isOplogBackupRunning() {
		return fmt.Errorf("Backup is not running")
	}
	// This should never happen. We get the last oplog timestamp when agents call DBBackupFinished
	if s.lastOplogTs == 0 {
		log.Errorf("Trying to stop the oplog tailer but last oplog timestamp is 0. Using current timestamp")
		s.lastOplogTs = time.Now().Unix()
	}

	var gErr error
	for _, client := range s.clients {
		s.logger.Debugf("Checking if client %s is running the backup: %v", client.NodeName, client.isOplogTailerRunning())
		if client.isOplogTailerRunning() {
			s.logger.Debugf("Stopping oplog tail in client %s at %s", client.NodeName, time.Unix(s.lastOplogTs, 0).Format(time.RFC3339))
			err := client.stopOplogTail(s.lastOplogTs)
			if err != nil {
				gErr = errors.Wrapf(gErr, "client: %s, error: %s", client.NodeName, err)
			}
		}
	}
	s.setBackupRunning(false)

	if gErr != nil {
		return errors.Wrap(gErr, "cannot stop oplog tailer")
	}
	return nil
}

func (s *MessagesServer) WaitBackupFinish() {
	replicasets := s.ReplicasetsRunningDBBackup()
	if len(replicasets) == 0 {
		return
	}
	<-s.dbBackupFinishChan
}

func (s *MessagesServer) WaitOplogBackupFinish() {
	replicasets := s.ReplicasetsRunningOplogBackup()
	if len(replicasets) == 0 {
		return
	}
	<-s.oplogBackupFinishChan
}

func (s *MessagesServer) WaitRestoreFinish() {
	replicasets := s.ReplicasetsRunningRestore()
	if len(replicasets) == 0 {
		return
	}
	<-s.restoreFinishChan
}

func (s *MessagesServer) WriteBackupMetadata(filename string) error {
	return s.lastBackupMetadata.WriteMetadataToFile(filepath.Join(s.workDir, filename))
}

// WorkDir returns the server working directory.
func (s *MessagesServer) WorkDir() string {
	return s.workDir
}

// ---------------------------------------------------------------------------------------------------------------------
//                                                              gRPC methods
// ---------------------------------------------------------------------------------------------------------------------

// DBBackupFinished process backup finished message from clients.
// After the mongodump call finishes, clients should call this method to inform the event to the server
func (s *MessagesServer) DBBackupFinished(ctx context.Context, msg *pb.DBBackupFinishStatus) (*pb.Ack, error) {
	if !msg.GetOK() {

	}

	s.lock.Lock()
	defer s.lock.Unlock()

	client := s.getClientByNodeName(msg.GetClientID())
	if client == nil {
		return nil, fmt.Errorf("Unknown client ID: %s", msg.GetClientID())
	}
	client.setDBBackupRunning(false)

	replicasets := s.ReplicasetsRunningDBBackup()

	// Keep the last (bigger) oplog timestamp from all clients running the backup.
	// When all clients finish the backup, we will call CloseAt(s.lastOplogTs) on all clients to have a consistent
	// stop time for all oplogs.
	lastOplogTs := msg.GetTs()
	if lastOplogTs > s.lastOplogTs {
		s.lastOplogTs = lastOplogTs
	}

	if len(replicasets) == 0 {
		notify.Post(EVENT_BACKUP_FINISH, time.Now())
	}
	return &pb.Ack{}, nil
}

// MessagesChat is the method exposed by gRPC to stream messages between the server and agents
func (s *MessagesServer) MessagesChat(stream pb.Messages_MessagesChatServer) error {
	msg, err := stream.Recv()
	if err != nil {
		return err
	}

	log.Debugf("Registering new client: %s", msg.GetClientID())
	client, err := s.registerClient(stream, msg)
	if err != nil {
		s.logger.Errorf("Cannot register client: %s", err)
		r := &pb.ServerMessage{
			Payload: &pb.ServerMessage_ErrorMsg{
				ErrorMsg: &pb.Error{
					Code:    pb.ErrorType_CLIENT_ALREADY_REGISTERED,
					Message: "",
				},
			},
		}
		err = stream.Send(r)

		return ClientAlreadyExistsError
	}
	s.clients[msg.ClientID] = client
	r := &pb.ServerMessage{
		Payload: &pb.ServerMessage_AckMsg{AckMsg: &pb.Ack{}},
	}

	if err := stream.Send(r); err != nil {
		return err
	}

	// Keep the stream open
	<-stream.Context().Done()

	return nil
}

// OplogBackupFinished process oplog tailer finished message from clients.
// After the the oplog tailer has been closed on clients, clients should call this method to inform the event to the server
func (s *MessagesServer) OplogBackupFinished(ctx context.Context, msg *pb.OplogBackupFinishStatus) (*pb.Ack, error) {
	client := s.getClientByNodeName(msg.GetClientID())
	if client == nil {
		return nil, fmt.Errorf("Unknown client ID: %s", msg.GetClientID())
	}
	client.setOplogTailerRunning(false)

	replicasets := s.ReplicasetsRunningOplogBackup()
	if len(replicasets) == 0 {
		notify.Post(EVENT_OPLOG_FINISH, time.Now())
	}
	return &pb.Ack{}, nil
}

// RestoreCompleted handles a replicaset restore completed messages from clients.
// After restore is completed or upon errors, each client running the restore will cann this gRPC method
// to inform the server about the restore status.
func (s *MessagesServer) RestoreCompleted(ctx context.Context, msg *pb.RestoreComplete) (*pb.Ack, error) {
	client := s.getClientByNodeName(msg.GetClientID())
	if client == nil {
		return nil, fmt.Errorf("Unknown client ID: %s", msg.GetClientID())
	}
	log.Infof("---> Received RestoreCompleted from client %v", msg.GetClientID())
	client.setRestoreRunning(false)

	replicasets := s.ReplicasetsRunningOplogBackup()
	if len(replicasets) == 0 {
		s.setRestoreRunning(false)
		notify.Post(EVENT_RESTORE_FINISH, time.Now())
	}
	return &pb.Ack{}, nil
}

// ---------------------------------------------------------------------------------------------------------------------
//                                                        Internal helpers
// ---------------------------------------------------------------------------------------------------------------------

func (s *MessagesServer) cancelBackup() error {
	if !s.isOplogBackupRunning() {
		return fmt.Errorf("Backup is not running")
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	var gerr error
	for _, client := range s.clients {
		if err := client.stopBackup(); err != nil {
			gerr = errors.Wrapf(err, "cannot stop backup on %s", client.ID)
		}
	}
	return gerr
}

func (s *MessagesServer) getClientByNodeName(name string) *Client {
	for _, client := range s.clients {
		if client.NodeName == name {
			return client
		}
	}
	return nil
}

func (s *MessagesServer) handleClientDisconnection() {
	for {
		clientID := <-s.clientDisconnetedChan
		log.Infof("Client %s has been disconnected", clientID)
		s.unregisterClient(clientID)
		// TODO: Check if backup is running to stop it or to tell a different client in the
		// same replicaset to start a new backup
	}
}

func (s *MessagesServer) isBackupRunning() bool {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.backupRunning
}

func (s *MessagesServer) isOplogBackupRunning() bool {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.oplogBackupRunning
}

func (s *MessagesServer) isRestoreRunning() bool {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.restoreRunning
}

func (s *MessagesServer) registerClient(stream pb.Messages_MessagesChatServer, msg *pb.ClientMessage) (*Client, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if msg.ClientID == "" {
		return nil, fmt.Errorf("Invalid client ID (empty)")
	}

	if client, exists := s.clients[msg.ClientID]; exists {
		if err := client.ping(); err != nil {
			s.unregisterClient(client.ID) // Since we already know the client ID is valid, it will never return an error
		} else {
			return nil, ClientAlreadyExistsError
		}
	}

	regMsg := msg.GetRegisterMsg()
	if regMsg == nil || regMsg.NodeType == pb.NodeType_UNDEFINED {
		return nil, fmt.Errorf("Node type in register payload cannot be empty")
	}
	s.logger.Debugf("Register msg: %+v", regMsg)
	client := newClient(msg.ClientID, regMsg.ClusterID, regMsg.NodeName, regMsg.ReplicasetID, regMsg.ReplicasetName,
		regMsg.NodeType, stream, s.logger, s.clientDisconnetedChan)
	s.clients[msg.ClientID] = client
	return client, nil
}

func (s *MessagesServer) reset() {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.lastOplogTs = 0
	s.backupRunning = false
	s.oplogBackupRunning = false
	s.restoreRunning = false
	s.err = nil
}

func (s *MessagesServer) setBackupRunning(status bool) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.backupRunning = status
}

func (s *MessagesServer) setOplogBackupRunning(status bool) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.oplogBackupRunning = status
}

func (s *MessagesServer) setRestoreRunning(status bool) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.restoreRunning = status
}

func (s *MessagesServer) unregisterClient(id string) error {
	log.Infof("Unregistering client %s", id)
	s.lock.Lock()
	defer s.lock.Unlock()

	if _, exists := s.clients[id]; !exists {
		return UnknownClientID
	}

	delete(s.clients, id)
	return nil
}
