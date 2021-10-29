package ncproxystore

import (
	"context"
	"encoding/json"

	"github.com/Microsoft/hcsshim/internal/ncproxynetworking"
	"github.com/pkg/errors"
	bolt "go.etcd.io/bbolt"
)

var (
	ErrBucketNotFound = errors.New("bucket not found")
	errKeyNotFound    = errors.New("key does not exist")
)

type NetworkingStore struct {
	db *bolt.DB
}

func NewNetworkingStore(database *bolt.DB) *NetworkingStore {
	return &NetworkingStore{
		db: database,
	}
}

func (n *NetworkingStore) Close() error {
	return n.db.Close()
}

func (n *NetworkingStore) GetNetworkByName(ctx context.Context, networkName string) (*ncproxynetworking.Network, error) {
	internalData := &ncproxynetworking.Network{}
	if err := n.db.View(func(tx *bolt.Tx) error {
		bkt := getNetworkBucket(tx)
		if bkt == nil {
			return errors.Wrapf(ErrBucketNotFound, "network bucket %v", bucketKeyNetwork)
		}
		data := bkt.Get([]byte(networkName))
		if data == nil {
			return errors.Wrapf(errKeyNotFound, "network %v", networkName)
		}
		if err := json.Unmarshal(data, internalData); err != nil {
			return errors.Wrapf(err, "data is %v", string(data))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return internalData, nil
}

func (n *NetworkingStore) CreateNetwork(ctx context.Context, network *ncproxynetworking.Network) error {
	if err := n.db.Update(func(tx *bolt.Tx) error {
		bkt, err := createNetworkBucket(tx)
		if err != nil {
			return err
		}
		internalData, err := json.Marshal(network)
		if err != nil {
			return err
		}
		return bkt.Put([]byte(network.NetworkName), internalData)
	}); err != nil {
		return err
	}
	return nil
}

func (n *NetworkingStore) DeleteNetwork(ctx context.Context, networkName string) error {
	if err := n.db.Update(func(tx *bolt.Tx) error {
		bkt := getNetworkBucket(tx)
		if bkt == nil {
			return errors.Wrapf(ErrBucketNotFound, "bucket %v", bucketKeyNetwork)
		}
		return bkt.Delete([]byte(networkName))
	}); err != nil {
		return err
	}
	return nil
}

func (n *NetworkingStore) ListNetworks(ctx context.Context) (results []*ncproxynetworking.Network, err error) {
	if err := n.db.View(func(tx *bolt.Tx) error {
		bkt := getNetworkBucket(tx)
		if bkt == nil {
			return errors.Wrapf(ErrBucketNotFound, "network bucket %v", bucketKeyNetwork)
		}
		err := bkt.ForEach(func(k, v []byte) error {
			data := bkt.Get([]byte(k))
			if data == nil {
				return errors.Wrapf(errKeyNotFound, "network %v", k)
			}
			internalData := &ncproxynetworking.Network{}
			if err := json.Unmarshal(data, internalData); err != nil {
				return errors.Wrapf(err, "data is %v", string(data))
			}
			results = append(results, internalData)
			return nil
		})
		return err
	}); err != nil {
		return nil, err
	}

	return results, nil
}

func (n *NetworkingStore) GetEndpointByName(ctx context.Context, endpointName string) (*ncproxynetworking.Endpoint, error) {
	endpt := &ncproxynetworking.Endpoint{}
	if err := n.db.View(func(tx *bolt.Tx) error {
		bkt := getEndpointBucket(tx)
		if bkt == nil {
			return errors.Wrapf(ErrBucketNotFound, "endpoint bucket %v", bucketKeyEndpoint)
		}
		jsonData := bkt.Get([]byte(endpointName))
		if jsonData == nil {
			return errors.Wrapf(errKeyNotFound, "endpoint %v", endpointName)
		}
		if err := json.Unmarshal(jsonData, endpt); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return endpt, nil
}

func (n *NetworkingStore) CreatEndpoint(ctx context.Context, endpt *ncproxynetworking.Endpoint) error {
	return n.updateEndpoint(ctx, endpt)
}

func (n *NetworkingStore) UpdateEndpoint(ctx context.Context, endpt *ncproxynetworking.Endpoint) error {
	return n.updateEndpoint(ctx, endpt)
}

func (n *NetworkingStore) updateEndpoint(ctx context.Context, endpt *ncproxynetworking.Endpoint) error {
	if err := n.db.Update(func(tx *bolt.Tx) error {
		bkt, err := createEndpointBucket(tx)
		if err != nil {
			return err
		}
		jsonEndptData, err := json.Marshal(endpt)
		if err != nil {
			return err
		}
		return bkt.Put([]byte(endpt.EndpointName), jsonEndptData)
	}); err != nil {
		return err
	}
	return nil
}

func (n *NetworkingStore) DeleteEndpoint(ctx context.Context, endpointName string) error {
	if err := n.db.Update(func(tx *bolt.Tx) error {
		bkt := getEndpointBucket(tx)
		if bkt == nil {
			return errors.Wrapf(ErrBucketNotFound, "bucket %v", bucketKeyEndpoint)
		}
		return bkt.Delete([]byte(endpointName))
	}); err != nil {
		return err
	}
	return nil
}

func (n *NetworkingStore) ListEndpoints(ctx context.Context) (results []*ncproxynetworking.Endpoint, err error) {
	if err := n.db.View(func(tx *bolt.Tx) error {
		bkt := getEndpointBucket(tx)
		if bkt == nil {
			return errors.Wrapf(ErrBucketNotFound, "endpoint bucket %v", bucketKeyEndpoint)
		}
		err := bkt.ForEach(func(k, v []byte) error {
			jsonData := bkt.Get([]byte(k))
			if jsonData == nil {
				return errors.Wrapf(errKeyNotFound, "endpoint %v", k)
			}
			endptInternal := &ncproxynetworking.Endpoint{}
			if err := json.Unmarshal(jsonData, endptInternal); err != nil {
				return err
			}
			results = append(results, endptInternal)
			return nil
		})
		return err
	}); err != nil {
		return nil, err
	}

	return results, nil
}

// ComputeAgentStore is a database that stores a key value pair of container id
// to compute agent server address
type ComputeAgentStore struct {
	DB *bolt.DB
}

func NewComputeAgentStore(db *bolt.DB) *ComputeAgentStore {
	return &ComputeAgentStore{DB: db}
}

func (c *ComputeAgentStore) Close() error {
	return c.DB.Close()
}

// GetComputeAgent returns the compute agent address of a single entry in the database for key `containerID`
// or returns an error if the key does not exist
func (c *ComputeAgentStore) GetComputeAgent(ctx context.Context, containerID string) (result string, err error) {
	if err := c.DB.View(func(tx *bolt.Tx) error {
		bkt := getComputeAgentBucket(tx)
		if bkt == nil {
			return errors.Wrapf(ErrBucketNotFound, "bucket %v", bucketKeyComputeAgent)
		}
		data := bkt.Get([]byte(containerID))
		if data == nil {
			return errors.Wrapf(errKeyNotFound, "key %v", containerID)
		}
		result = string(data)
		return nil
	}); err != nil {
		return "", err
	}

	return result, nil
}

// GetComputeAgents returns a map of the key value pairs stored in the database
// where the keys are the containerIDs and the values are the corresponding compute agent
// server addresses
func (c *ComputeAgentStore) GetComputeAgents(ctx context.Context) (map[string]string, error) {
	content := map[string]string{}
	if err := c.DB.View(func(tx *bolt.Tx) error {
		bkt := getComputeAgentBucket(tx)
		if bkt == nil {
			return errors.Wrapf(ErrBucketNotFound, "bucket %v", bucketKeyComputeAgent)
		}
		err := bkt.ForEach(func(k, v []byte) error {
			data := bkt.Get([]byte(k))
			content[string(k)] = string(data)
			return nil
		})
		return err
	}); err != nil {
		return nil, err
	}
	return content, nil
}

// UpdateComputeAgent updates or adds an entry (if none already exists) to the database
// `address` corresponds to the address of the compute agent server for the `containerID`
func (c *ComputeAgentStore) UpdateComputeAgent(ctx context.Context, containerID string, address string) error {
	if err := c.DB.Update(func(tx *bolt.Tx) error {
		bkt, err := createComputeAgentBucket(tx)
		if err != nil {
			return err
		}
		return bkt.Put([]byte(containerID), []byte(address))
	}); err != nil {
		return err
	}
	return nil
}

// DeleteComputeAgent deletes an entry in the database or returns an error if none exists
// `containerID` corresponds to the target key that the entry should be deleted for
func (c *ComputeAgentStore) DeleteComputeAgent(ctx context.Context, containerID string) error {
	if err := c.DB.Update(func(tx *bolt.Tx) error {
		bkt := getComputeAgentBucket(tx)
		if bkt == nil {
			return errors.Wrapf(ErrBucketNotFound, "bucket %v", bucketKeyComputeAgent)
		}
		return bkt.Delete([]byte(containerID))
	}); err != nil {
		return err
	}
	return nil
}
