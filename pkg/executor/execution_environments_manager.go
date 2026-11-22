package executor

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"

	"github.com/VaalaCat/openvaala/pkg/defs"
)

// ExecutionEnvironmentsManager 保存对话的在线执行环境。由 Conversation.Run 串行写入，允许并发读取。
type ExecutionEnvironmentsManager interface {
	// GetEnvironments 返回环境列表副本和默认环境 ID。稳定环境按配置顺序在前，动态环境按 ID 排序在后。
	GetEnvironments() (environments []ExecutionEnvironment, defaultID string)
	// GetEnvironment 按 ID 查询在线环境。
	GetEnvironment(id string) (environment ExecutionEnvironment, found bool)
	// SetStatic 替换稳定环境。默认环境必须排首位；空列表的默认 ID 为空。返回因 ID 冲突被移出的动态环境，由调用方停止。
	SetStatic(defaultID string, environments []ExecutionEnvironment) (removedDynamicIDs []string, err error)
	// SetDynamic 登记或替换动态环境并绑定连接标识。稳定环境 ID 和 memory 不可用于动态登记。
	SetDynamic(environment ExecutionEnvironment, connectionID string) (err error)
	// RemoveDynamic 仅移除连接标识仍与登记一致的动态环境。
	RemoveDynamic(id string, connectionID string) (removed bool)
}

type executionEnvironmentsManagerImpl struct {
	environments atomic.Pointer[managedEnvironments]
}

type managedEnvironments struct {
	static    []ExecutionEnvironment
	dynamic   []dynamicEnvironmentRegistration
	defaultID string
}

type dynamicEnvironmentRegistration struct {
	environment  ExecutionEnvironment
	connectionID string
}

func NewExecutionEnvironmentsManager() ExecutionEnvironmentsManager {
	manager := &executionEnvironmentsManagerImpl{}
	manager.environments.Store(&managedEnvironments{})
	return manager
}

func (m *executionEnvironmentsManagerImpl) GetEnvironments() ([]ExecutionEnvironment, string) {
	managed := m.environments.Load()
	environments := make([]ExecutionEnvironment, 0, len(managed.static)+len(managed.dynamic))
	environments = append(environments, managed.static...)
	for _, registration := range managed.dynamic {
		environments = append(environments, registration.environment)
	}
	return environments, managed.defaultID
}

func (m *executionEnvironmentsManagerImpl) GetEnvironment(id string) (ExecutionEnvironment, bool) {
	managed := m.environments.Load()
	for _, environment := range managed.static {
		if environment.GetID() == id {
			return environment, true
		}
	}
	for _, registration := range managed.dynamic {
		if registration.environment.GetID() == id {
			return registration.environment, true
		}
	}
	return nil, false
}

func (m *executionEnvironmentsManagerImpl) SetStatic(defaultID string, environments []ExecutionEnvironment) ([]string, error) {
	// 1. 校验环境类型、ID 和默认环境。
	staticIDs := make(map[string]struct{}, len(environments))
	for _, environment := range environments {
		if environment == nil {
			return nil, errors.New("execution environment is missing")
		}
		id := environment.GetID()
		if environment.IsDynamic() {
			return nil, fmt.Errorf("executor %q is dynamic", id)
		}
		if _, exists := staticIDs[id]; exists {
			return nil, fmt.Errorf("executor %q is defined more than once", id)
		}
		staticIDs[id] = struct{}{}
	}
	if len(environments) == 0 && defaultID != "" {
		return nil, errors.New("default executor requires a static environment")
	}
	if len(environments) > 0 && (defaultID == "" || environments[0].GetID() != defaultID) {
		return nil, errors.New("default executor must be the first static environment")
	}

	// 2. 移出撞名的动态环境，发布新的环境列表。
	next := &managedEnvironments{
		static:    slices.Clone(environments),
		defaultID: defaultID,
	}
	var removedDynamicIDs []string
	for _, registration := range m.environments.Load().dynamic {
		id := registration.environment.GetID()
		if _, exists := staticIDs[id]; exists {
			removedDynamicIDs = append(removedDynamicIDs, id)
			continue
		}
		next.dynamic = append(next.dynamic, registration)
	}
	m.environments.Store(next)
	return removedDynamicIDs, nil
}

func (m *executionEnvironmentsManagerImpl) SetDynamic(environment ExecutionEnvironment, connectionID string) error {
	// 1. 校验环境和连接标识，拒绝保留 ID 和稳定环境 ID。
	if environment == nil {
		return errors.New("execution environment is missing")
	}
	id := environment.GetID()
	if !environment.IsDynamic() {
		return fmt.Errorf("executor %q is not dynamic", id)
	}
	if connectionID == "" {
		return fmt.Errorf("dynamic executor %q requires a connection ID", id)
	}
	if id == defs.MemoryExecutorID {
		return fmt.Errorf("executor ID %q is reserved", id)
	}
	managed := m.environments.Load()
	for _, static := range managed.static {
		if static.GetID() == id {
			return fmt.Errorf("executor %q conflicts with a static environment", id)
		}
	}

	// 2. 按 ID 替换或插入登记，发布新的环境列表。
	next := *managed
	next.dynamic = slices.Clone(managed.dynamic)
	registration := dynamicEnvironmentRegistration{
		environment:  environment,
		connectionID: connectionID,
	}
	index, found := slices.BinarySearchFunc(next.dynamic, id, func(registration dynamicEnvironmentRegistration, id string) int {
		return cmp.Compare(registration.environment.GetID(), id)
	})
	if found {
		next.dynamic[index] = registration
	} else {
		next.dynamic = slices.Insert(next.dynamic, index, registration)
	}
	m.environments.Store(&next)
	return nil
}

func (m *executionEnvironmentsManagerImpl) RemoveDynamic(id string, connectionID string) bool {
	// 1. 确认连接标识仍匹配后删除登记并发布。
	managed := m.environments.Load()
	index := slices.IndexFunc(managed.dynamic, func(registration dynamicEnvironmentRegistration) bool {
		return registration.environment.GetID() == id && registration.connectionID == connectionID
	})
	if index < 0 {
		return false
	}
	next := *managed
	next.dynamic = slices.Delete(slices.Clone(managed.dynamic), index, index+1)
	m.environments.Store(&next)
	return true
}
