// SPDX-FileCopyrightText: 2020-present Open Networking Foundation <info@opennetworking.org>
//
// SPDX-License-Identifier: Apache-2.0

// Package gnmi implements a gnmi server to mock a device with YANG models.
package gnmi

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	pb "github.com/openconfig/gnmi/proto/gnmi"
	"github.com/openconfig/goyang/pkg/yang"
	"github.com/openconfig/ygot/ygot"
	"github.com/openconfig/ygot/ytypes"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// doDelete deletes the path from the json tree if the path exists. If success,
// it calls the callback function to apply the change to the device hardware.
func (s *Server) doDelete(jsonTree map[string]interface{}, prefix, path *pb.Path) (*pb.UpdateResult, bool, error) {
	// Update json tree of the device config
	var curNode interface{} = jsonTree
	pathDeleted := false
	fullPath := gnmiFullPath(prefix, path)
	schema := s.model.schemaTreeRoot
	for i, elem := range fullPath.Elem { // Delete sub-tree or leaf node.
		node, ok := curNode.(map[string]interface{})
		if !ok {
			log.Warnf("Failed to map node %v", curNode)
			break
		}

		// Delete node
		if i == len(fullPath.Elem)-1 {
			if elem.GetKey() == nil {
				delete(node, elem.Name)
				pathDeleted = true
				break
			}
			pathDeleted = deleteKeyedListEntry(node, elem)
			if !pathDeleted {
				log.Warnf("deleteKeyedListEntry returned false on node=%v, elem=%v", node, elem)
			}
			break
		}

		if curNode, schema = getChildNode(node, schema, elem, false); curNode == nil {
			log.Warnf("Delete stopping due to no child, node=%v, elem=%v", node, elem)
			break
		}
	}
	if reflect.DeepEqual(fullPath, pbRootPath) { // Delete root
		for k := range jsonTree {
			delete(jsonTree, k)
		}
	}

	if pathDeleted {
		if s.callback != nil {
			// Note that s.config has not received the changes yet, so it still contains
			// the object being deleted, and can be used to lookup information about
			// it inside the callback.
			log.Debugf("Calling delete callback on: %s", PathToString(fullPath))
			err := s.callback(s.config, Deleted, fullPath)
			if err != nil {
				return nil, false, err
			}
		}
		log.Infof("Deleted: %s", PathToString(fullPath))
	}

	return &pb.UpdateResult{
		Path: path,
		Op:   pb.UpdateResult_DELETE,
	}, pathDeleted, nil
}

// doReplaceOrUpdate validates the replace or update operation to be applied to
// the device, modifies the json tree of the config struct, then calls the
// callback function to apply the operation to the device hardware.
func (s *Server) doReplaceOrUpdate(jsonTree map[string]interface{}, op pb.UpdateResult_Operation, prefix, path *pb.Path, val *pb.TypedValue) (*pb.UpdateResult, error) {
	fullPath := gnmiFullPath(prefix, path)

	var nodeVal interface{}

	// Validate the operation.
	emptyNode, entry, err := ytypes.GetOrCreateNode(s.model.schemaTreeRoot, s.model.newRootValue(), fullPath)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "path %v is not found in the config structure: %v", fullPath, err)
	}

	nodeStruct, ok := emptyNode.(ygot.ValidatedGoStruct)
	if ok {
		if err := s.model.jsonUnmarshaler(val.GetJsonIetfVal(), nodeStruct); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "unmarshaling json data to config struct fails: %v", err)
		}
		if err := nodeStruct.Validate(); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "config data validation fails: %v", err)
		}
		var err error
		if nodeVal, err = ygot.ConstructIETFJSON(nodeStruct, &ygot.RFC7951JSONConfig{}); err != nil {
			msg := fmt.Sprintf("error in constructing IETF JSON tree from config struct: %v", err)
			log.Error(msg)
			return nil, status.Error(codes.Internal, msg)
		}
	} else {
		// If the Yang entry is a uint64, then we need to store it as a string in the JSON Tree
		// instead of as a uint.
		intAsString := (entry.Type != nil) && ((entry.Type.Kind == yang.Yuint64) || (entry.Type.Kind == yang.Yint64))
		nodeVal, err = convertTypedValueToJSONValue(val, intAsString)
		if err != nil {
			return nil, err
		}
		log.Infof("Update/replace: %s = %v (%T)",
			PrefixAndPathToString(prefix, path),
			nodeVal,
			nodeVal)
	}

	// Update json tree of the device config.
	var curNode interface{} = jsonTree
	schema := s.model.schemaTreeRoot
	for i, elem := range fullPath.Elem {
		switch node := curNode.(type) {
		case map[string]interface{}:
			// Set node value.
			if i == len(fullPath.Elem)-1 {
				if elem.GetKey() == nil {
					if grpcStatusError := setPathWithoutAttribute(op, node, elem, nodeVal); grpcStatusError != nil {
						return nil, grpcStatusError
					}
					break
				}
				if grpcStatusError := setPathWithAttribute(op, node, elem, nodeVal); grpcStatusError != nil {
					return nil, grpcStatusError
				}
				break
			}

			if curNode, schema = getChildNode(node, schema, elem, true); curNode == nil {
				return nil, status.Errorf(codes.NotFound, "path elem not found: %v", elem)
			}
		case []interface{}:
			return nil, status.Errorf(codes.NotFound, "incompatible path elem: %v", elem)
		default:
			return nil, status.Errorf(codes.Internal, "wrong node type: %T", curNode)
		}
	}
	if reflect.DeepEqual(fullPath, pbRootPath) { // Replace/Update root.
		if op == pb.UpdateResult_UPDATE {
			return nil, status.Error(codes.Unimplemented, "update the root of config tree is unsupported")
		}
		nodeValAsTree, ok := nodeVal.(map[string]interface{})
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "expect a tree to replace the root, got a scalar value: %T", nodeVal)
		}
		for k := range jsonTree {
			delete(jsonTree, k)
		}
		for k, v := range nodeValAsTree {
			jsonTree[k] = v
		}
	}

	return &pb.UpdateResult{
		Path: path,
		Op:   op,
	}, nil
}

// Set implements the Set RPC in gNMI spec.
func (s *Server) Set(req *pb.SetRequest) (*pb.SetResponse, error) {
	tStart := time.Now()
	gnmiRequestsTotal.WithLabelValues("SET").Inc()

	s.mu.Lock()
	defer s.mu.Unlock()

	jsonTree, err := ygot.ConstructIETFJSON(s.config, &ygot.RFC7951JSONConfig{})
	if err != nil {
		msg := fmt.Sprintf("error in constructing IETF JSON tree from config struct: %v", err)
		log.Error(msg)
		gnmiRequestsFailedTotal.WithLabelValues("SET").Inc()
		return nil, status.Error(codes.Internal, msg)
	}

	prefix := req.GetPrefix()
	var results []*pb.UpdateResult

	for _, path := range req.GetDelete() {
		log.Debugf("Handling delete: %v", path)
		res, _, grpcStatusError := s.doDelete(jsonTree, prefix, path)
		if grpcStatusError != nil {
			log.Warnf("Delete returning with error %v", grpcStatusError)
			gnmiRequestsFailedTotal.WithLabelValues("SET").Inc()
			return nil, grpcStatusError
		}
		results = append(results, res)
	}
	for _, upd := range req.GetReplace() {
		log.Debugf("Handling replace: %v", upd)
		res, grpcStatusError := s.doReplaceOrUpdate(jsonTree, pb.UpdateResult_REPLACE, prefix, upd.GetPath(), upd.GetVal())
		if grpcStatusError != nil {
			gnmiRequestsFailedTotal.WithLabelValues("SET").Inc()
			log.Warnf("Replace returning with error %v", grpcStatusError)
			return nil, grpcStatusError
		}
		results = append(results, res)
	}
	for _, upd := range req.GetUpdate() {
		log.Debugf("Handling update: %v", upd)
		res, grpcStatusError := s.doReplaceOrUpdate(jsonTree, pb.UpdateResult_UPDATE, prefix, upd.GetPath(), upd.GetVal())
		if grpcStatusError != nil {
			gnmiRequestsFailedTotal.WithLabelValues("SET").Inc()
			log.Warnf("Update returning with error %v", grpcStatusError)
			return nil, grpcStatusError
		}
		results = append(results, res)
	}

	jsonDump, err := json.Marshal(jsonTree)
	if err != nil {
		msg := fmt.Sprintf("error in marshaling IETF JSON tree to bytes: %v", err)
		log.Error(msg)
		gnmiRequestsFailedTotal.WithLabelValues("SET").Inc()
		return nil, status.Error(codes.Internal, msg)
	}

	rootStruct, err := s.model.NewConfigStruct(jsonDump)
	if err != nil {
		msg := fmt.Sprintf("error in creating config struct from IETF JSON data: %v", err)
		log.Error(msg)
		gnmiRequestsFailedTotal.WithLabelValues("SET").Inc()
		return nil, status.Error(codes.Internal, msg)
	}

	// Apply the validated operation to the device.
	// Note: We apply this after all operations have been applied to the config tree, because it is
	// more performant to the json.Marshal and NewConfigStruct once per gnmi operation than it is to
	// do it for each individual path set or delete.
	if s.callback != nil {
		if applyErr := s.callback(rootStruct, Apply, nil); applyErr != nil {
			if rollbackErr := s.callback(s.config, Rollback, nil); rollbackErr != nil {
				return nil, status.Errorf(codes.Internal, "error in rollback the failed operation (%v): %v", applyErr, rollbackErr)
			}
			return nil, status.Errorf(codes.Aborted, "error in applying operation to device: %v", applyErr)
		}
	}

	s.config = rootStruct

	setResponse := &pb.SetResponse{
		Prefix:   req.GetPrefix(),
		Response: results,
	}

	for _, response := range setResponse.GetResponse() {
		update := &pb.Update{
			Path: response.GetPath(),
		}
		s.ConfigUpdate.In() <- update
	}

	gnmiRequestDuration.WithLabelValues("SET").Observe(time.Since(tStart).Seconds())

	return setResponse, nil
}
