// Copyright 2020 Douyu
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

package etcdv3

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/etcd/etcdserver/api/v3rpc/rpctypes"
	"github.com/douyu/jupiter/pkg"
	"github.com/douyu/jupiter/pkg/constant"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/mvcc/mvccpb"
	"github.com/douyu/jupiter/pkg/client/etcdv3"
	"github.com/douyu/jupiter/pkg/ecode"
	"github.com/douyu/jupiter/pkg/registry"
	"github.com/douyu/jupiter/pkg/server"
	"github.com/douyu/jupiter/pkg/util/xgo"
	"github.com/douyu/jupiter/pkg/xlog"
)

type etcdv3Registry struct {
	client *etcdv3.Client
	kvs    sync.Map
	*Config
	cancel context.CancelFunc
	leases map[string]clientv3.LeaseID
	rmu    *sync.RWMutex
}

func newETCDRegistry(config *Config) *etcdv3Registry {
	if config.logger == nil {
		config.logger = xlog.JupiterLogger
	}
	config.logger = config.logger.With(xlog.FieldMod(ecode.ModRegistryETCD), xlog.FieldAddrAny(config.Config.Endpoints))
	reg := &etcdv3Registry{
		client: config.Config.Build(),
		Config: config,
		kvs:    sync.Map{},
		leases: make(map[string]clientv3.LeaseID),
		rmu:    &sync.RWMutex{},
	}
	return reg
}

// RegisterService register service to registry
func (reg *etcdv3Registry) RegisterService(ctx context.Context, info *server.ServiceInfo) error {
	err := reg.registerBiz(ctx, info)
	if err != nil {
		return err
	}
	return reg.registerMetric(ctx, info)
}

// UnregisterService unregister service from registry
func (reg *etcdv3Registry) UnregisterService(ctx context.Context, info *server.ServiceInfo) error {
	return reg.unregister(ctx, reg.registerKey(info))
}

// ListServices list service registered in registry with name `name`
func (reg *etcdv3Registry) ListServices(ctx context.Context, name string, scheme string) (services []*server.ServiceInfo, err error) {
	target := fmt.Sprintf("/%s/%s/providers/%s://", reg.Prefix, name, scheme)
	getResp, getErr := reg.client.Get(ctx, target, clientv3.WithPrefix())
	if getErr != nil {
		reg.logger.Error(ecode.MsgWatchRequestErr, xlog.FieldErrKind(ecode.ErrKindRequestErr), xlog.FieldErr(getErr), xlog.FieldAddr(target))
		return nil, getErr
	}

	for _, kv := range getResp.Kvs {
		var service server.ServiceInfo
		if err := json.Unmarshal(kv.Value, &service); err != nil {
			reg.logger.Warnf("invalid service", xlog.FieldErr(err))
			continue
		}
		services = append(services, &service)
	}

	return
}

// WatchServices watch service change event, then return address list
func (reg *etcdv3Registry) WatchServices(ctx context.Context, name string, scheme string) (chan registry.Endpoints, error) {
	prefix := fmt.Sprintf("/%s/%s/", reg.Prefix, name)
	watch, err := reg.client.WatchPrefix(context.Background(), prefix)
	if err != nil {
		return nil, err
	}

	var addresses = make(chan registry.Endpoints, 10)
	var al = &registry.Endpoints{
		Nodes:           make(map[string]server.ServiceInfo),
		RouteConfigs:    make(map[string]registry.RouteConfig),
		ConsumerConfigs: make(map[string]registry.ConsumerConfig),
		ProviderConfigs: make(map[string]registry.ProviderConfig),
	}

	for _, kv := range watch.IncipientKeyValues() {
		updateAddrList(al, prefix, scheme, kv)
	}

	addresses <- *al

	xgo.Go(func() {
		for event := range watch.C() {
			al2 := reg.cloneEndPoints(al)
			switch event.Type {
			case mvccpb.PUT:
				updateAddrList(al2, prefix, scheme, event.Kv)
			case mvccpb.DELETE:
				deleteAddrList(al2, prefix, scheme, event.Kv)
			}

			select {
			case addresses <- *al2:
			default:
				xlog.Warnf("invalid")
			}
		}
	})

	return addresses, nil
}

func (reg *etcdv3Registry) cloneEndPoints(src *registry.Endpoints) *registry.Endpoints {
	dst := &registry.Endpoints{
		Nodes:           make(map[string]server.ServiceInfo),
		RouteConfigs:    make(map[string]registry.RouteConfig),
		ConsumerConfigs: make(map[string]registry.ConsumerConfig),
		ProviderConfigs: make(map[string]registry.ProviderConfig),
	}
	for k, v := range src.Nodes {
		dst.Nodes[k] = v
	}
	for k, v := range src.RouteConfigs {
		dst.RouteConfigs[k] = v
	}
	for k, v := range src.ConsumerConfigs {
		dst.ConsumerConfigs[k] = v
	}
	for k, v := range src.ProviderConfigs {
		dst.ProviderConfigs[k] = v
	}
	return dst
}

func (reg *etcdv3Registry) unregister(ctx context.Context, key string) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, reg.ReadTimeout)
		defer cancel()
	}

	if err := reg.delLeaseID(ctx, key); err != nil {
		return err
	}

	_, err := reg.client.Delete(ctx, key)
	if err == nil {
		reg.kvs.Delete(key)
	}
	return err
}

// Close ...
func (reg *etcdv3Registry) Close() error {
	if reg.cancel != nil {
		reg.cancel()
	}
	var wg sync.WaitGroup
	reg.kvs.Range(func(k, v interface{}) bool {
		wg.Add(1)
		go func(k interface{}) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := reg.unregister(ctx, k.(string))
			if err != nil {
				reg.logger.Error("unregister service", xlog.FieldErrKind(ecode.ErrKindRequestErr), xlog.FieldErr(err), xlog.FieldErr(err), xlog.FieldKeyAny(k), xlog.FieldValueAny(v))
			} else {
				reg.logger.Info("unregister service", xlog.FieldKeyAny(k), xlog.FieldValueAny(v))
			}
			cancel()
		}(k)
		return true
	})
	wg.Wait()
	return nil
}

func (reg *etcdv3Registry) registerMetric(ctx context.Context, info *server.ServiceInfo) error {
	if info.Kind != constant.ServiceGovernor {
		return nil
	}

	metric := "/prometheus/job/%s/%s"

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, reg.ReadTimeout)
		defer cancel()
	}

	key := fmt.Sprintf(metric, info.Name, pkg.HostName())
	val := info.Address

	opOptions := make([]clientv3.OpOption, 0)
	// opOptions = append(opOptions, clientv3.WithSerializable())
	if reg.Config.ServiceTTL > 0 {
		leaseID, err := reg.getLeaseID(ctx, key)
		if err != nil {
			return err
		}
		opOptions = append(opOptions, clientv3.WithLease(leaseID))
		//KeepAlive ctx without timeout for same as service life
		reg.keepLeaseID(ctx, leaseID)
	}
	_, err := reg.client.Put(ctx, key, val, opOptions...)
	if err != nil {
		reg.logger.Error("register service", xlog.FieldErrKind(ecode.ErrKindRegisterErr), xlog.FieldErr(err), xlog.FieldKeyAny(key), xlog.FieldValueAny(info))
		return err
	}

	reg.logger.Info("register service", xlog.FieldKeyAny(key), xlog.FieldValueAny(val))
	reg.kvs.Store(key, val)
	return nil

}
func (reg *etcdv3Registry) getLeaseID(ctx context.Context, k string) (clientv3.LeaseID, error) {
	reg.rmu.RLock()
	leaseID, ok := reg.leases[k]
	reg.rmu.RUnlock()
	if ok {
		//from map try keep alive once
		if _, err := reg.client.KeepAliveOnce(ctx, leaseID); err != nil {
			if err == rpctypes.ErrLeaseNotFound {
				goto grant
			}
			return leaseID, err
		}
		return leaseID, nil
	}
grant:
	//grant
	rsp, err := reg.client.Grant(ctx, int64(reg.Config.ServiceTTL.Seconds()))
	if err != nil {
		return leaseID, err
	}
	//cache to map
	reg.rmu.Lock()
	reg.leases[k] = rsp.ID
	reg.rmu.Unlock()
	return rsp.ID, nil
}
func (reg *etcdv3Registry) keepLeaseID(ctx context.Context, leaseID clientv3.LeaseID) {
	go func() {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		ch, err := reg.client.KeepAlive(ctx, leaseID)
		if err != nil {
			return
		}
		for {
			select {
			case lkp := <-ch:
				if lkp == nil {
					return
				}
			}
		}
	}()
}
func (reg *etcdv3Registry) delLeaseID(ctx context.Context, k string) error {
	if reg.Config.ServiceTTL > 0 {
		reg.rmu.Lock()
		id, ok := reg.leases[k]
		delete(reg.leases, k)
		reg.rmu.Unlock()
		if ok {
			if _, err := reg.client.Revoke(ctx, id); err != nil {
				return err
			}
		}
	}
	return nil
}
func (reg *etcdv3Registry) registerBiz(ctx context.Context, info *server.ServiceInfo) error {
	var readCtx context.Context
	var readCancel context.CancelFunc
	if _, ok := ctx.Deadline(); !ok {
		readCtx, readCancel = context.WithTimeout(ctx, reg.ReadTimeout)
		defer readCancel()
	}

	key := reg.registerKey(info)
	val := reg.registerValue(info)

	opOptions := make([]clientv3.OpOption, 0)
	// opOptions = append(opOptions, clientv3.WithSerializable())
	if reg.Config.ServiceTTL > 0 {
		leaseID, err := reg.getLeaseID(readCtx, key)
		if err != nil {
			return err
		}
		opOptions = append(opOptions, clientv3.WithLease(leaseID))
		//KeepAlive ctx without timeout for same as service life
		reg.keepLeaseID(ctx, leaseID)
	}
	_, err := reg.client.Put(readCtx, key, val, opOptions...)
	if err != nil {
		reg.logger.Error("register service", xlog.FieldErrKind(ecode.ErrKindRegisterErr), xlog.FieldErr(err), xlog.FieldKeyAny(key), xlog.FieldValueAny(info))
		return err
	}
	reg.logger.Info("register service", xlog.FieldKeyAny(key), xlog.FieldValueAny(val))
	reg.kvs.Store(key, val)
	return nil

}

func (reg *etcdv3Registry) registerKey(info *server.ServiceInfo) string {
	return registry.GetServiceKey(reg.Prefix, info)
}

func (reg *etcdv3Registry) registerValue(info *server.ServiceInfo) string {
	return registry.GetServiceValue(info)
}

func deleteAddrList(al *registry.Endpoints, prefix, scheme string, kvs ...*mvccpb.KeyValue) {
	for _, kv := range kvs {
		var addr = strings.TrimPrefix(string(kv.Key), prefix)
		if strings.HasPrefix(addr, "providers/"+scheme) {
			// 解析服务注册键
			addr = strings.TrimPrefix(addr, "providers/")
			if addr == "" {
				continue
			}
			uri, err := url.Parse(addr)
			if err != nil {
				xlog.Error("parse uri", xlog.FieldErrKind(ecode.ErrKindUriErr), xlog.FieldErr(err), xlog.FieldKey(string(kv.Key)))
				continue
			}
			delete(al.Nodes, uri.String())
		}

		if strings.HasPrefix(addr, "configurators/"+scheme) {
			// 解析服务配置键
			addr = strings.TrimPrefix(addr, "configurators/")
			if addr == "" {
				continue
			}
			uri, err := url.Parse(addr)
			if err != nil {
				xlog.Error("parse uri", xlog.FieldErrKind(ecode.ErrKindUriErr), xlog.FieldErr(err), xlog.FieldKey(string(kv.Key)))
				continue
			}
			delete(al.RouteConfigs, uri.String())
		}

		if isIPPort(addr) {
			// 直接删除addr 因为Delete操作的value值为空
			delete(al.Nodes, addr)
			delete(al.RouteConfigs, addr)
		}
	}
}

func updateAddrList(al *registry.Endpoints, prefix, scheme string, kvs ...*mvccpb.KeyValue) {
	for _, kv := range kvs {
		var addr = strings.TrimPrefix(string(kv.Key), prefix)
		switch {
		// 解析服务注册键
		case strings.HasPrefix(addr, "providers/"+scheme):
			addr = strings.TrimPrefix(addr, "providers/")
			uri, err := url.Parse(addr)
			if err != nil {
				xlog.Error("parse uri", xlog.FieldErrKind(ecode.ErrKindUriErr), xlog.FieldErr(err), xlog.FieldKey(string(kv.Key)))
				continue
			}
			var serviceInfo server.ServiceInfo
			if err := json.Unmarshal(kv.Value, &serviceInfo); err != nil {
				xlog.Error("parse uri", xlog.FieldErrKind(ecode.ErrKindUriErr), xlog.FieldErr(err), xlog.FieldKey(string(kv.Key)))
				continue
			}
			al.Nodes[uri.String()] = serviceInfo
		case strings.HasPrefix(addr, "configurators/"+scheme):
			addr = strings.TrimPrefix(addr, "configurators/")

			uri, err := url.Parse(addr)
			if err != nil {
				xlog.Error("parse uri", xlog.FieldErrKind(ecode.ErrKindUriErr), xlog.FieldErr(err), xlog.FieldKey(string(kv.Key)))
				continue
			}

			if strings.HasPrefix(uri.Path, "/routes/") { // 路由配置
				var routeConfig registry.RouteConfig
				if err := json.Unmarshal(kv.Value, &routeConfig); err != nil {
					xlog.Error("parse uri", xlog.FieldErrKind(ecode.ErrKindUriErr), xlog.FieldErr(err), xlog.FieldKey(string(kv.Key)))
					continue
				}
				routeConfig.ID = strings.TrimPrefix(uri.Path, "/routes/")
				routeConfig.Scheme = uri.Scheme
				routeConfig.Host = uri.Host
				al.RouteConfigs[uri.String()] = routeConfig
			}

			if strings.HasPrefix(uri.Path, "/providers/") {
				var providerConfig registry.ProviderConfig
				if err := json.Unmarshal(kv.Value, &providerConfig); err != nil {
					xlog.Error("parse uri", xlog.FieldErrKind(ecode.ErrKindUriErr), xlog.FieldErr(err), xlog.FieldKey(string(kv.Key)))
					continue
				}
				providerConfig.ID = strings.TrimPrefix(uri.Path, "/providers/")
				providerConfig.Scheme = uri.Scheme
				providerConfig.Host = uri.Host
				al.ProviderConfigs[uri.String()] = providerConfig
			}

			if strings.HasPrefix(uri.Path, "/consumers/") {
				var consumerConfig registry.ConsumerConfig
				if err := json.Unmarshal(kv.Value, &consumerConfig); err != nil {
					xlog.Error("parse uri", xlog.FieldErrKind(ecode.ErrKindUriErr), xlog.FieldErr(err), xlog.FieldKey(string(kv.Key)))
					continue
				}
				consumerConfig.ID = strings.TrimPrefix(uri.Path, "/consumers/")
				consumerConfig.Scheme = uri.Scheme
				consumerConfig.Host = uri.Host
				al.ConsumerConfigs[uri.String()] = consumerConfig
			}
		}
	}
}

func isIPPort(addr string) bool {
	_, _, err := net.SplitHostPort(addr)
	return err == nil
}

/*
key: /jupiter/main/configurator/grpc:///routes/1
val:
{
	"upstream": { // 客户端配置
		"nodes": { // 按照node负载均衡
			"127.0.0.1:1980": 1,
			"127.0.0.1:1981": 4
		},
		"group": { // 按照group负载均衡
			"red": 2,
			"green": 1
		}
	},
	"uri": "/hello",
	"deployment": "open_api"
}

key: /jupiter/main/configurator/grpc://127.0.0.1/routes/2
val:
{
	"upstream": { // 客户端配置
		"nodes": { // 按照node负载均衡
			"127.0.0.1:1980": 1,
			"127.0.0.1:1981": 1
		},
		"group": { // 按照group负载均衡
			"red": 1,
			"green": 2
		}
	},
	"uri": "/hello",
	"deployment": "core_api" // 部署组
}

key: /jupiter/main/configurator/grpc:///consumers/client-demo
val:
{

}
*/
