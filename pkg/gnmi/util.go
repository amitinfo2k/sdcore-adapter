// SPDX-FileCopyrightText: 2020-present Open Networking Foundation <info@opennetworking.org>
//
// SPDX-License-Identifier: Apache-2.0

// Package gnmi implements a gnmi server to mock a device with YANG models.
package gnmi

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang/protobuf/proto" //nolint: staticcheck
	dpb "github.com/golang/protobuf/protoc-gen-go/descriptor"
	"github.com/openconfig/goyang/pkg/yang"
	"github.com/openconfig/ygot/ygot"
	"github.com/openconfig/ygot/ytypes"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/openconfig/gnmi/proto/gnmi"
	pb "github.com/openconfig/gnmi/proto/gnmi"
	"github.com/openconfig/gnmi/value"
)

// getGNMIServiceVersion returns a pointer to the gNMI service version string.
// The method is non-trivial because of the way it is defined in the proto file.
func getGNMIServiceVersion() (*string, error) {
	gzB, _ := (&pb.Update{}).Descriptor() // nolint:staticcheck
	r, err := gzip.NewReader(bytes.NewReader(gzB))
	if err != nil {
		return nil, fmt.Errorf("error in initializing gzip reader: %v", err)
	}
	defer r.Close()
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("error in reading gzip data: %v", err)
	}
	desc := &dpb.FileDescriptorProto{}
	if err := proto.Unmarshal(b, desc); err != nil {
		return nil, fmt.Errorf("error in unmarshaling proto: %v", err)
	}
	ver, err := proto.GetExtension(desc.Options, pb.E_GnmiService)
	if err != nil {
		return nil, fmt.Errorf("error in getting version from proto extension: %v", err)
	}
	return ver.(*string), nil
}

// tryChoices checks to see if elemName is behind a choice.
// An Example is:
//    gNMI: slice.mbr
//    schema: slice.bitrate.mbr-case.mbr
func tryChoices(schema *yang.Entry, elemName string) *yang.Entry {
	for _, entry := range schema.Dir {
		// Check each entry in Schema to see if it's a choice
		if entry.IsChoice() {
			// If it's a choice, then check each subentry to see if it's
			// a choice option with the field we're looking for hanging
			// off it.
			for _, subEntry := range entry.Dir {
				nextSchema, ok := subEntry.Dir[elemName]
				if ok {
					return nextSchema
				}
			}
		}
	}
	return nil
}

// getChildNode gets a node's child with corresponding schema specified by path
// element. If not found and createIfNotExist is set as true, an empty node is
// created and returned.
func getChildNode(node map[string]interface{}, schema *yang.Entry, elem *pb.PathElem, createIfNotExist bool) (interface{}, *yang.Entry) {
	var nextSchema *yang.Entry
	var ok bool

	if nextSchema, ok = schema.Dir[elem.Name]; !ok {
		nextSchema = tryChoices(schema, elem.Name)
		if nextSchema == nil {
			return nil, nil
		}
	}

	var nextNode interface{}
	if elem.GetKey() == nil {
		if nextNode, ok = node[elem.Name]; !ok {
			if createIfNotExist {
				node[elem.Name] = make(map[string]interface{})
				nextNode = node[elem.Name]
			}
		}
		return nextNode, nextSchema
	}

	nextNode = getKeyedListEntry(node, elem, createIfNotExist)
	return nextNode, nextSchema
}

// getKeyedListEntry finds the keyed list entry in node by the name and key of
// path elem. If entry is not found and createIfNotExist is true, an empty entry
// will be created (the list will be created if necessary).
func getKeyedListEntry(node map[string]interface{}, elem *pb.PathElem, createIfNotExist bool) map[string]interface{} {
	curNode, ok := node[elem.Name]
	if !ok {
		if !createIfNotExist {
			return nil
		}

		// Create a keyed list as node child and initialize an entry.
		m := make(map[string]interface{})
		for k, v := range elem.Key {
			m[k] = v
			if vAsNum, err := strconv.ParseFloat(v, 64); err == nil {
				m[k] = vAsNum
			}
		}
		node[elem.Name] = []interface{}{m}
		return m
	}

	// Search entry in keyed list.
	keyedList, ok := curNode.([]interface{})
	if !ok {
		return nil
	}
	for _, n := range keyedList {
		m, ok := n.(map[string]interface{})
		if !ok {
			log.Errorf("wrong keyed list entry type: %T", n)
			return nil
		}
		keyMatching := true
		// must be exactly match
		for k, v := range elem.Key {
			attrVal, ok := m[k]
			if !ok {
				return nil
			}
			if v != fmt.Sprintf("%v", attrVal) {
				keyMatching = false
				break
			}
		}
		if keyMatching {
			return m
		}
	}
	if !createIfNotExist {
		log.Warnf("Key %v not found in keyedList %v", elem, keyedList)
		return nil
	}

	// Create an entry in keyed list.
	m := make(map[string]interface{})
	for k, v := range elem.Key {
		m[k] = v
		if vAsNum, err := strconv.ParseFloat(v, 64); err == nil {
			m[k] = vAsNum
		}
	}
	node[elem.Name] = append(keyedList, m)
	return m
}

// gnmiFullPath builds the full path from the prefix and path.
func gnmiFullPath(prefix, path *pb.Path) *pb.Path {
	fullPath := &pb.Path{Origin: path.Origin}
	if path.GetElement() != nil { // nolint:staticcheck
		fullPath.Element = append(prefix.GetElement(), path.GetElement()...) // nolint:staticcheck
	}
	if path.GetElem() != nil { // nolint:staticcheck
		fullPath.Elem = append(prefix.GetElem(), path.GetElem()...)
	}
	return fullPath
}

// PathToString converts a gnmi path to a human-readable string
func PathToString(path *pb.Path) string {
	if path == nil {
		return "(nil)"
	}
	parts := []string{}
	for _, elem := range path.Elem {
		keys := []string{}
		for k, v := range elem.Key {
			keys = append(keys, fmt.Sprintf("%s=%s", k, v))
		}
		if len(keys) > 0 {
			parts = append(parts, fmt.Sprintf("%s[%s]", elem.Name, strings.Join(keys, ",")))
		} else {
			parts = append(parts, elem.Name)
		}
	}
	return strings.Join(parts, "/")
}

// PrefixAndPathToString converts a gnmi prefix and path to a human-readable string
func PrefixAndPathToString(prefix, path *pb.Path) string {
	if prefix == nil && path == nil {
		return "(nil)"
	} else if prefix == nil {
		return PathToString(path)
	} else if path == nil {
		return PathToString(prefix)
	} else {
		return fmt.Sprintf("%s/%s", PathToString(prefix), PathToString(path))
	}
}

// isNIl checks if an interface is nil or its value is nil.
func isNil(i interface{}) bool {
	if i == nil {
		return true
	}
	switch kind := reflect.ValueOf(i).Kind(); kind {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return reflect.ValueOf(i).IsNil()
	default:
		return false
	}
}

// checkEncodingAndModel checks whether encoding and models are supported by the server. Return error if anything is unsupported.
func (s *Server) checkEncodingAndModel(encoding pb.Encoding, models []*pb.ModelData) error {
	hasSupportedEncoding := false
	for _, supportedEncoding := range supportedEncodings {
		if encoding == supportedEncoding {
			hasSupportedEncoding = true
			break
		}
	}
	if !hasSupportedEncoding {
		return fmt.Errorf("unsupported encoding: %s", pb.Encoding_name[int32(encoding)])
	}
	for _, m := range models {
		isSupported := false
		for _, supportedModel := range s.model.modelData {
			if reflect.DeepEqual(m, supportedModel) {
				isSupported = true
				break
			}
		}
		if !isSupported {
			return fmt.Errorf("unsupported model: %v", m)
		}
	}
	return nil
}

// GetConfig returns the config store
func (s *Server) GetConfig() (ygot.ValidatedGoStruct, error) {
	return s.config, nil
}

// deleteKeyedListEntry deletes the keyed list entry from node that matches the
// path elem. If the entry is the only one in keyed list, deletes the entire
// list. If the entry is found and deleted, the function returns true. If it is
// not found, the function returns false.
func deleteKeyedListEntry(node map[string]interface{}, elem *pb.PathElem) bool {
	curNode, ok := node[elem.Name]
	if !ok {
		return false
	}

	keyedList, ok := curNode.([]interface{})
	if !ok {
		return false
	}
	for i, n := range keyedList {
		m, ok := n.(map[string]interface{})
		if !ok {
			log.Errorf("expect map[string]interface{} for a keyed list entry, got %T", n)
			return false
		}
		keyMatching := true
		for k, v := range elem.Key {
			attrVal, ok := m[k]
			if !ok {
				return false
			}
			if v != fmt.Sprintf("%v", attrVal) {
				keyMatching = false
				break
			}
		}
		if keyMatching {
			listLen := len(keyedList)
			if listLen == 1 {
				delete(node, elem.Name)
				return true
			}
			keyedList[i] = keyedList[listLen-1]
			node[elem.Name] = keyedList[0 : listLen-1]
			return true
		}
	}
	return false
}

// setPathWithAttribute replaces or updates a child node of curNode in the IETF
// JSON config tree, where the child node is indexed by pathElem with attribute.
// The function returns grpc status error if unsuccessful.
func setPathWithAttribute(op pb.UpdateResult_Operation, curNode map[string]interface{}, pathElem *pb.PathElem, nodeVal interface{}) error {
	nodeValAsTree, ok := nodeVal.(map[string]interface{})
	if !ok {
		return status.Errorf(codes.InvalidArgument, "expect nodeVal is a json node of map[string]interface{}, received %T", nodeVal)
	}
	m := getKeyedListEntry(curNode, pathElem, true)
	if m == nil {
		return status.Errorf(codes.NotFound, "path elem not found: %v", pathElem)
	}
	if op == pb.UpdateResult_REPLACE {
		for k := range m {
			delete(m, k)
		}
	}
	for attrKey, attrVal := range pathElem.GetKey() {
		m[attrKey] = attrVal
		if asNum, err := strconv.ParseFloat(attrVal, 64); err == nil {
			m[attrKey] = asNum
		}
		for k, v := range nodeValAsTree {
			if k == attrKey && fmt.Sprintf("%v", v) != attrVal {
				return status.Errorf(codes.InvalidArgument, "invalid config data: %v is a path attribute", k)
			}
		}
	}
	for k, v := range nodeValAsTree {
		m[k] = v
	}
	return nil
}

// setPathWithoutAttribute replaces or updates a child node of curNode in the
// IETF config tree, where the child node is indexed by pathElem without
// attribute. The function returns grpc status error if unsuccessful.
func setPathWithoutAttribute(op pb.UpdateResult_Operation, curNode map[string]interface{}, pathElem *pb.PathElem, nodeVal interface{}) error {
	target, hasElem := curNode[pathElem.Name]
	nodeValAsTree, nodeValIsTree := nodeVal.(map[string]interface{})
	if op == pb.UpdateResult_REPLACE || !hasElem || !nodeValIsTree {
		curNode[pathElem.Name] = nodeVal
		return nil
	}
	targetAsTree, ok := target.(map[string]interface{})
	if !ok {
		return status.Errorf(codes.Internal, "error in setting path: expect map[string]interface{} to update, got %T", target)
	}
	for k, v := range nodeValAsTree {
		targetAsTree[k] = v
	}
	return nil
}

// sendResponse sends an SubscribeResponse to a gNMI client.
func (s *Server) sendResponse(response *pb.SubscribeResponse, stream pb.GNMI_SubscribeServer) {
	log.Debugf("Sending SubscribeResponse out to gNMI client: %s", response)
	err := stream.Send(response)
	if err != nil {
		//TODO remove channel registrations
		log.Errorf("Error in sending response to client %v", err)
	}
}

// getUpdate finds the node in the tree, build the update message and return it back to the collector
func (s *Server) getUpdate(c *streamClient, subList *pb.SubscriptionList, path *pb.Path) (*pb.Update, error) {

	fullPath := path
	prefix := subList.GetPrefix()
	if prefix != nil {
		fullPath = gnmiFullPath(prefix, path)
	}
	if fullPath.GetElem() == nil && fullPath.GetElement() != nil { // nolint:staticcheck
		return nil, status.Error(codes.Unimplemented, "deprecated path element type is unsupported")
	}
	node, err := ytypes.GetNode(s.model.schemaTreeRoot, s.config, fullPath)
	if isNil(node) || err != nil {
		return nil, status.Errorf(codes.NotFound, "path %v not found", fullPath)

	}

	nodeStruct, ok := node[0].Data.(ygot.GoStruct)
	// Return leaf node.
	if !ok {
		var val *pb.TypedValue
		switch kind := reflect.ValueOf(node).Kind(); kind {
		case reflect.Ptr, reflect.Interface:
			var err error
			val, err = value.FromScalar(reflect.ValueOf(node).Elem().Interface())
			if err != nil {
				msg := fmt.Sprintf("leaf node %v does not contain a scalar type value: %v", path, err)
				log.Error(msg)
				return nil, status.Error(codes.Internal, msg)
			}
		case reflect.Int64:
			enumMap, ok := s.model.enumData[reflect.TypeOf(node).Name()]
			if !ok {
				return nil, status.Error(codes.Internal, "not a GoStruct enumeration type")

			}
			val = &pb.TypedValue{
				Value: &pb.TypedValue_StringVal{
					StringVal: enumMap[reflect.ValueOf(node).Int()].Name,
				},
			}
		case reflect.Slice:
			var err error
			switch kind := reflect.ValueOf(node[0].Data).Kind(); kind {
			case reflect.Int64:
				enumMap, ok := s.model.enumData[reflect.TypeOf(node[0].Data).Name()]
				if !ok {
					return nil, status.Error(codes.Internal, "not a GoStruct enumeration type")
				}
				val = &pb.TypedValue{
					Value: &pb.TypedValue_StringVal{
						StringVal: enumMap[reflect.ValueOf(node[0].Data).Int()].Name,
					},
				}
			default:
				val, err = value.FromScalar(reflect.ValueOf(node[0].Data).Elem().Interface())
				if err != nil {
					msg := fmt.Sprintf("leaf node %v does not contain a scalar type value: %v", fullPath, err)
					log.Error(msg)
					return nil, status.Error(codes.Internal, msg)
				}
			}
		default:
			return nil, status.Errorf(codes.Internal, "unexpected kind of leaf node type: %v %v", node, kind)
		}

		update := &pb.Update{Path: path, Val: val}
		return update, nil

	}

	// Return IETF JSON for the sub-tree.
	jsonTree, err := ygot.ConstructIETFJSON(nodeStruct, &ygot.RFC7951JSONConfig{AppendModuleName: true})
	if err != nil {
		msg := fmt.Sprintf("error in constructing IETF JSON tree from requested node: %v", err)
		log.Error(msg)
		return nil, status.Error(codes.Internal, msg)
	}
	jsonDump, err := json.Marshal(jsonTree)
	if err != nil {
		msg := fmt.Sprintf("error in marshaling IETF JSON tree to bytes: %v", err)
		log.Error(msg)
		return nil, status.Error(codes.Internal, msg)
	}
	update := &pb.Update{
		Path: path,
		Val: &pb.TypedValue{
			Value: &pb.TypedValue_JsonIetfVal{
				JsonIetfVal: jsonDump,
			},
		},
	}

	return update, nil

}

// collector collects the latest update from the config.
func (s *Server) collector(c *streamClient, request *pb.SubscriptionList) {
	for _, sub := range request.Subscription {
		path := sub.GetPath()
		update, err := s.getUpdate(c, request, path)

		if err != nil {
			log.Warnf("Error while collecting data for subscribe once or poll: %s", err)
			update = &pb.Update{
				Path: path,
			}
			c.UpdateChan <- update
		}

		if err == nil {
			c.UpdateChan <- update
		}
	}
}

// listenForUpdates reads update messages from the update channel, creates a
// subscribe response and send it to the gnmi client.
func (s *Server) listenForUpdates(c *streamClient) {
	for update := range c.UpdateChan {
		if update.Val == nil {
			deleteResponse := buildDeleteResponse(update.GetPath())
			s.sendResponse(deleteResponse, c.stream)
			syncResponse := buildSyncResponse()
			s.sendResponse(syncResponse, c.stream)

		} else {
			response, _ := buildSubResponse(update)
			s.sendResponse(response, c.stream)
			syncResponse := buildSyncResponse()
			s.sendResponse(syncResponse, c.stream)
		}
	}
}

// configEventProducer produces update events for stream subscribed.
func (s *Server) listenToConfigEvents(request *pb.SubscriptionList) {
	for updateInterface := range s.ConfigUpdate.Out() {
		update := updateInterface.(*pb.Update)
		for key, clientList := range s.subscribed {
			if key == update.GetPath().String() {
				for _, c := range clientList {
					newUpdateValue, err := s.getUpdate(c, request, update.GetPath())

					if err != nil {
						deleteResponse := buildDeleteResponse(update.GetPath())
						s.sendResponse(deleteResponse, c.stream)
						syncResponse := buildSyncResponse()
						s.sendResponse(syncResponse, c.stream)

					} else {
						update.Val = newUpdateValue.Val

						// builds subscription response
						response, _ := buildSubResponse(update)

						s.sendResponse(response, c.stream)
						// builds Sync response
						syncResponse := buildSyncResponse()
						s.sendResponse(syncResponse, c.stream)
					}
				}
			}
		}
	}

}

// buildSubResponse builds a subscribeResponse based on the given Update message.
func buildSubResponse(update *pb.Update) (*pb.SubscribeResponse, error) {
	updateArray := make([]*pb.Update, 0)
	updateArray = append(updateArray, update)
	notification := &pb.Notification{
		Timestamp: time.Now().Unix(),
		Update:    updateArray,
	}
	responseUpdate := &pb.SubscribeResponse_Update{
		Update: notification,
	}
	response := &pb.SubscribeResponse{
		Response: responseUpdate,
	}

	return response, nil
}

// buildDeleteResponse builds a subscribe response for the given deleted path.
func buildDeleteResponse(delete *pb.Path) *gnmi.SubscribeResponse {
	deleteArray := []*gnmi.Path{delete}
	notification := &gnmi.Notification{
		Timestamp: time.Now().Unix(),
		Delete:    deleteArray,
	}
	responseUpdate := &gnmi.SubscribeResponse_Update{
		Update: notification,
	}
	response := &gnmi.SubscribeResponse{
		Response: responseUpdate,
	}
	return response
}

// buildSyncResponse builds a sync response.
func buildSyncResponse() *gnmi.SubscribeResponse {
	responseSync := &gnmi.SubscribeResponse_SyncResponse{
		SyncResponse: true,
	}
	return &gnmi.SubscribeResponse{
		Response: responseSync,
	}
}

// Contains checks the existence of a given string in an array of strings.
func Contains(a []string, x string) bool {
	for _, n := range a {
		if x == n {
			return true
		}
	}
	return false
}

// pruneConfigData prunes the given JSON subtree based on the given data type and path info.
func pruneConfigData(data interface{}, dataType string, fullPath *pb.Path) interface{} {

	if reflect.ValueOf(data).Kind() == reflect.Slice {
		d := reflect.ValueOf(data)
		tmpData := make([]interface{}, d.Len())
		returnSlice := make([]interface{}, d.Len())
		for i := 0; i < d.Len(); i++ {
			tmpData[i] = d.Index(i).Interface()
		}
		for i, v := range tmpData {
			returnSlice[i] = pruneConfigData(v, dataType, fullPath)
		}
		return returnSlice
	} else if reflect.ValueOf(data).Kind() == reflect.Map {
		d := reflect.ValueOf(data)
		tmpData := make(map[string]interface{})
		for _, k := range d.MapKeys() {
			match, _ := regexp.MatchString(dataType, k.String())
			matchAll := strings.Compare(dataType, "all")
			typeOfValue := reflect.TypeOf(d.MapIndex(k).Interface()).Kind()

			if match || matchAll == 0 {
				newKey := k.String()
				if typeOfValue == reflect.Map || typeOfValue == reflect.Slice {
					tmpData[newKey] = pruneConfigData(d.MapIndex(k).Interface(), dataType, fullPath)

				} else {
					tmpData[newKey] = d.MapIndex(k).Interface()
				}
			} else {
				tmpIteration := pruneConfigData(d.MapIndex(k).Interface(), dataType, fullPath)
				if typeOfValue == reflect.Map {
					tmpMap := tmpIteration.(map[string]interface{})
					if len(tmpMap) != 0 {
						tmpData[k.String()] = tmpIteration
						if Contains(dataTypes, k.String()) {
							delete(tmpData, k.String())
						}
					}
				} else if typeOfValue == reflect.Slice {
					tmpMap := tmpIteration.([]interface{})
					if len(tmpMap) != 0 {
						tmpData[k.String()] = tmpIteration
						if Contains(dataTypes, k.String()) {
							delete(tmpData, k.String())

						}
					}
				} else {
					tmpData[k.String()] = d.MapIndex(k).Interface()

				}
			}

		}

		return tmpData
	}
	return data
}

func buildUpdate(b []byte, path *pb.Path, valType string) *pb.Update {
	var update *pb.Update

	if strings.Compare(valType, "Internal") == 0 {
		update = &pb.Update{Path: path, Val: &pb.TypedValue{Value: &pb.TypedValue_JsonVal{JsonVal: b}}}
		return update
	}
	update = &pb.Update{Path: path, Val: &pb.TypedValue{Value: &pb.TypedValue_JsonIetfVal{JsonIetfVal: b}}}

	return update
}

func jsonEncoder(encoderType string, nodeStruct ygot.GoStruct) (map[string]interface{}, error) {

	if strings.Compare(encoderType, "Internal") == 0 {
		return ygot.ConstructInternalJSON(nodeStruct)
	}

	return ygot.ConstructIETFJSON(nodeStruct, &ygot.RFC7951JSONConfig{AppendModuleName: true})

}

/*
 * JSON requires that 64-bit integer values be encoded as strings. We rely on the caller to
 * let us know whether the destination needs to be converted to a string.
 */

func convertTypedValueToJSONValue(val *pb.TypedValue, intAsString bool) (interface{}, error) {
	var err error
	var nodeVal interface{}

	switch val.Value.(type) {
	case *pb.TypedValue_UintVal:
		u := val.GetUintVal()
		if intAsString {
			nodeVal = strconv.FormatUint(u, 10)
		} else {
			nodeVal = u
		}
	case *pb.TypedValue_IntVal:
		i := val.GetIntVal()
		if intAsString {
			nodeVal = strconv.FormatInt(i, 10)
		} else {
			nodeVal = i
		}
	default:
		if nodeVal, err = value.ToScalar(val); err != nil {
			return nil, status.Errorf(codes.Internal, "cannot convert leaf node to scalar type: %v", err)
		}
	}

	return nodeVal, nil
}
