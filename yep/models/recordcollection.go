// Copyright 2016 NDP Systèmes. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package models

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/npiganeau/yep/yep/models/security"
	"github.com/npiganeau/yep/yep/models/types"
)

// RecordCollection is a generic struct representing several
// records of a model.
type RecordCollection struct {
	model     *Model
	callStack []*methodLayer
	query     *Query
	env       *Environment
	ids       []int64
	fetched   bool
	filtered  bool
}

// String returns the string representation of a RecordSet
func (rc RecordCollection) String() string {
	idsStr := make([]string, len(rc.ids))
	for i, id := range rc.ids {
		idsStr[i] = strconv.Itoa(int(id))
		i++
	}
	rsIds := strings.Join(idsStr, ",")
	return fmt.Sprintf("%s(%s)", rc.model.name, rsIds)
}

// Env returns the RecordSet's Environment
func (rc RecordCollection) Env() Environment {
	res := *rc.env
	return res
}

// ModelName returns the model name of the RecordSet
func (rc RecordCollection) ModelName() string {
	return rc.model.name
}

// Ids returns the ids of the RecordSet, fetching from db if necessary.
func (rc RecordCollection) Ids() []int64 {
	rSet := rc.Fetch()
	return rSet.ids
}

// create inserts a new record in the database with the given data.
// data can be either a FieldMap or a struct pointer of the same model as rs.
// This function is private and low level. It should not be called directly.
// Instead use rs.Call("Create")
func (rc RecordCollection) create(data interface{}) RecordCollection {
	mustCheckModelPermission(rc.model, rc.env.uid, security.Create)
	fMap := ConvertInterfaceToFieldMap(data)
	fMap = filterMapOnAuthorizedFields(rc.model, fMap, rc.env.uid, security.Create)
	rc.applyDefaults(&fMap)
	rc.addAccessFieldsCreateData(&fMap)
	rc.model.convertValuesToFieldType(&fMap)
	fMap = rc.createEmbeddedRecords(fMap)
	// clean our fMap from ID and non stored fields
	fMap.RemovePKIfZero()
	storedFieldMap := filterMapOnStoredFields(rc.model, fMap)
	// insert in DB
	var createdId int64
	sql, args := rc.query.insertQuery(storedFieldMap)
	rc.env.cr.Get(&createdId, sql, args...)

	rSet := rc.withIds([]int64{createdId})
	// update reverse relation fields
	rSet.updateRelationFields(fMap)
	// compute stored fields
	rSet.updateStoredFields(fMap)
	return rSet
}

// createEmbeddedRecords creates the records that are embedded in this
// one if they don't already exist. It returns the given fMap with the
// ids inserted for the embedded records.
func (rc RecordCollection) createEmbeddedRecords(fMap FieldMap) FieldMap {
	type modelAndValues struct {
		model  string
		values FieldMap
	}
	embeddedData := make(map[string]modelAndValues)
	// 1. We create entries in our map for each embedded field if they don't already have an id
	for fName, fi := range rc.model.fields.registryByName {
		if !fi.embed {
			continue
		}
		if id, ok := fMap[fi.json].(int64); ok && id != int64(0) {
			continue
		}
		embeddedData[fName] = modelAndValues{
			model:  fi.relatedModelName,
			values: make(FieldMap),
		}
	}
	// 2. We populate our map with the values for each embedded record
	for fName, value := range fMap {
		fi := rc.Model().fields.mustGet(fName)
		if fi.relatedPath == "" {
			continue
		}
		exprs := strings.Split(fi.relatedPath, ExprSep)
		if len(exprs) != 2 {
			continue
		}
		fm, ok := embeddedData[exprs[0]]
		if !ok {
			continue
		}
		fm.values[exprs[1]] = value
	}
	// 3. We create the embedded records
	for fieldName, vals := range embeddedData {
		fMap[fieldName] = rc.env.Pool(vals.model).create(vals.values).ids[0]
	}
	return fMap
}

// applyDefaults adds the default value to the given fMap values which
// are equal to their Go type zero value
func (rc RecordCollection) applyDefaults(fMap *FieldMap) {
	for fName, fi := range Registry.MustGet(rc.ModelName()).fields.registryByJSON {
		if fi.defaultFunc == nil {
			continue
		}
		val := reflect.ValueOf((*fMap)[fName])
		if !val.IsValid() || val == reflect.Zero(val.Type()) {
			(*fMap)[fName] = fi.defaultFunc(rc.Env(), FieldMap{})
		}
	}
}

// addAccessFieldsCreateData adds appropriate CreateDate and CreateUID fields to
// the given FieldMap.
func (rc RecordCollection) addAccessFieldsCreateData(fMap *FieldMap) {
	if !rc.model.isSystem() {
		(*fMap)["CreateDate"] = Now()
		(*fMap)["CreateUID"] = rc.env.uid
	}
}

// update updates the database with the given data and returns the number of updated rows.
// It panics in case of error.
// This function is private and low level. It should not be called directly.
// Instead use rs.Call("Write")
func (rc RecordCollection) update(data interface{}, fieldsToUnset ...FieldNamer) bool {
	mustCheckModelPermission(rc.model, rc.env.uid, security.Write)
	rSet := rc.addRecordRuleConditions(rc.env.uid, security.Write)
	fMap := ConvertInterfaceToFieldMap(data)
	if _, ok := data.(FieldMap); !ok {
		for _, f := range fieldsToUnset {
			if _, exists := fMap[string(f.FieldName())]; !exists {
				fMap[string(f.FieldName())] = nil
			}
		}
	}
	rSet.addAccessFieldsUpdateData(&fMap)
	rSet.model.convertValuesToFieldType(&fMap)
	// clean our fMap from ID and non stored fields
	fMap.RemovePK()
	storedFieldMap := filterMapOnStoredFields(rSet.model, fMap)
	rSet.doUpdate(storedFieldMap)
	// Let's fetch once for all
	rSet = rSet.Fetch()
	// write reverse relation fields
	rSet.updateRelationFields(fMap)
	// write related fields
	rSet.updateRelatedFields(fMap)
	// compute stored fields
	rSet.updateStoredFields(fMap)
	return true
}

// addAccessFieldsUpdateData adds appropriate WriteDate and WriteUID fields to
// the given FieldMap.
func (rc RecordCollection) addAccessFieldsUpdateData(fMap *FieldMap) {
	if !rc.model.isSystem() {
		(*fMap)["WriteDate"] = Now()
		(*fMap)["WriteUID"] = rc.env.uid
	}
}

// doUpdate just updates the database records pointed at by
// this RecordCollection with the given fieldMap. It also
// invalidates the cache for the record
func (rc RecordCollection) doUpdate(fMap FieldMap) {
	defer func() {
		for _, id := range rc.Ids() {
			rc.env.cache.invalidateRecord(rc.model, id)
		}
	}()
	fMap = filterMapOnAuthorizedFields(rc.model, fMap, rc.env.uid, security.Write)
	// update DB
	if len(fMap) > 0 {
		sql, args := rc.query.updateQuery(fMap)
		res := rc.env.cr.Execute(sql, args...)
		if num, _ := res.RowsAffected(); num == 0 {
			log.Panic("Trying to update an empty RecordSet", "model", rc.ModelName(), "values", fMap)
		}
	}
}

// updateRelationFields updates reverse relations fields of the
// given fMap.
func (rc RecordCollection) updateRelationFields(fMap FieldMap) {
	rSet := rc.Fetch()
	for field, value := range fMap {
		fi := rc.model.getRelatedFieldInfo(field)
		if !checkFieldPermission(fi, rc.env.uid, security.Write) {
			continue
		}
		switch fi.fieldType {
		case types.One2Many:
		case types.Rev2One:
		case types.Many2Many:
			delQuery := fmt.Sprintf(`DELETE FROM %s WHERE %s IN (?)`, fi.m2mRelModel.tableName, fi.m2mOurField.json)
			rc.env.cr.Execute(delQuery, rSet.ids)
			for _, id := range rSet.ids {
				query := fmt.Sprintf(`INSERT INTO %s (%s, %s) VALUES (?, ?)`, fi.m2mRelModel.tableName,
					fi.m2mOurField.json, fi.m2mTheirField.json)
				for _, relId := range value.([]int64) {
					rc.env.cr.Execute(query, id, relId)
				}
			}
		}
	}
}

// updateRelatedFields updates related non stored fields of the
// given fMap.
func (rc RecordCollection) updateRelatedFields(fMap FieldMap) {
	rSet := rc.Fetch()
	var toLoad []string
	toSubstitute := make(map[string]string)
	for field := range fMap {
		fi := rSet.model.fields.mustGet(field)
		if !fi.isRelatedField() {
			continue
		}
		if !checkFieldPermission(fi, rc.env.uid, security.Write) {
			continue
		}

		toSubstitute[field] = fi.relatedPath
		if !rSet.env.cache.checkIfInCache(rSet.model, rSet.ids, []string{fi.relatedPath}) {
			toLoad = append(toLoad, field)
		}
	}
	// Load related paths if not loaded already
	if len(toLoad) > 0 {
		rSet = rSet.Load(toLoad...)
	}
	// Create an update map for each record to update
	updateMap := make(map[RecordRef]FieldMap)
	for _, rec := range rSet.Records() {
		for field, subst := range toSubstitute {
			ref, relField, _ := rec.env.cache.getRelatedRef(rec.model, rec.ids[0], subst)
			if _, exists := updateMap[ref]; !exists {
				updateMap[ref] = make(FieldMap)
			}
			updateMap[ref][relField] = fMap[field]
		}
	}

	// Make the update for each record
	for ref, upMap := range updateMap {
		rs := rc.env.Pool(ref.ModelName).withIds([]int64{ref.ID})
		rs.doUpdate(upMap)
	}
}

// delete deletes the database record of this RecordSet and returns the number of deleted rows.
// This function is private and low level. It should not be called directly.
// Instead use rs.Unlink() or rs.Call("Unlink")
func (rc RecordCollection) delete() int64 {
	mustCheckModelPermission(rc.model, rc.env.uid, security.Unlink)
	rSet := rc.addRecordRuleConditions(rc.env.uid, security.Unlink)
	sql, args := rSet.query.deleteQuery()
	res := rSet.env.cr.Execute(sql, args...)
	num, _ := res.RowsAffected()
	return num
}

// Search returns a new RecordSet filtering on the current one with the
// additional given Condition
func (rc RecordCollection) Search(cond *Condition) RecordCollection {
	rc.query = rc.query.clone()
	rc.query.cond = rc.query.cond.AndCond(cond)
	return rc
}

// Limit returns a new RecordSet with only the first 'limit' records.
func (rc RecordCollection) Limit(limit int) RecordCollection {
	rc.query = rc.query.clone()
	rc.query.limit = limit
	return rc
}

// Offset returns a new RecordSet with only the records starting at offset
func (rc RecordCollection) Offset(offset int) RecordCollection {
	rc.query = rc.query.clone()
	rc.query.offset = offset
	return rc
}

// OrderBy returns a new RecordSet ordered by the given ORDER BY expressions
func (rc RecordCollection) OrderBy(exprs ...string) RecordCollection {
	rc.query = rc.query.clone()
	rc.query.orders = append(rc.query.orders, exprs...)
	return rc
}

// GroupBy returns a new RecordSet grouped with the given GROUP BY expressions
func (rc RecordCollection) GroupBy(fields ...FieldNamer) RecordCollection {
	rc.query = rc.query.clone()
	exprs := make([]string, len(fields))
	for i, f := range fields {
		exprs[i] = string(f.FieldName())
	}
	rc.query.groups = append(rc.query.groups, exprs...)
	return rc
}

// Fetch query the database with the current filter and returns a RecordSet
// with the queries ids. Fetch is lazy and only return ids. Use Load() instead
// if you want to fetch all fields.
func (rc RecordCollection) Fetch() RecordCollection {
	if !rc.fetched && !rc.query.isEmpty() {
		// We do not load empty queries to keep empty record sets empty
		// Call FetchAll instead to load all the records of the table
		return rc.Load("id")
	}
	return rc
}

// FetchAll returns a RecordSet with all items of the table, regardless of the
// current RecordSet query. It is mainly meant to be used on an empty RecordSet
func (rc RecordCollection) FetchAll() RecordCollection {
	rSet := rc.env.Pool(rc.ModelName())
	rSet.query.fetchAll = true
	return rSet
}

// SearchCount fetch from the database the number of records that match the RecordSet conditions
// It panics in case of error
func (rc RecordCollection) SearchCount() int {
	rSet := rc.Limit(0)
	sql, args := rSet.query.countQuery()
	var res int
	rSet.env.cr.Get(&res, sql, args...)
	return res
}

// Load query all data of the RecordCollection and store in cache.
// fields are the fields to retrieve in the expression format,
// i.e. "User.Profile.Age" or "user_id.profile_id.age".
// If no fields are given, all DB columns of the RecordCollection's
// model are retrieved. Non-DB fields must be explicitly given in
// fields to be retrieved.
func (rc RecordCollection) Load(fields ...string) RecordCollection {
	if rc.query.isEmpty() {
		// Never load RecordSets without query.
		return rc
	}
	if len(rc.query.groups) > 0 {
		log.Panic("Trying to load a grouped query", "model", rc.model, "groups", rc.query.groups)
	}
	mustCheckModelPermission(rc.model, rc.env.uid, security.Read)
	rSet := rc.addRecordRuleConditions(rc.env.uid, security.Read)
	var results []FieldMap
	if len(fields) == 0 {
		fields = rSet.model.fields.storedFieldNames()
	}
	fields = filterOnAuthorizedFields(rSet.model, rSet.env.uid, fields, security.Read)
	subFields, rSet := rSet.substituteRelatedFields(fields)
	dbFields := filterOnDBFields(rSet.model, subFields)
	sql, args := rSet.query.selectQuery(dbFields)
	rows := dbQuery(rSet.env.cr.tx, sql, args...)
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		line := make(FieldMap)
		err := rSet.model.scanToFieldMap(rows, &line)
		if err != nil {
			log.Panic(err.Error(), "model", rSet.ModelName(), "fields", fields)
		}
		results = append(results, line)
		rSet.env.cache.addRecord(rSet.model, line["id"].(int64), line)
		ids = append(ids, line["id"].(int64))
	}

	rSet = rSet.withIds(ids)
	rSet.loadRelationFields(fields)
	return rSet
}

// loadRelationFields loads one2many, many2many and rev2one fields from the given fields
// names in this RecordCollection into the cache. fields of other types given in fields
// are ignored.
func (rc RecordCollection) loadRelationFields(fields []string) {
	for _, id := range rc.ids {
		for _, fieldName := range fields {
			fi := rc.model.getRelatedFieldInfo(fieldName)
			switch fi.fieldType {
			case types.One2Many:
				relRC := rc.env.Pool(fi.relatedModelName).Search(rc.Model().Field(fi.reverseFK).Equals(id)).Fetch()
				rc.env.cache.addEntry(rc.model, id, fieldName, relRC.ids)
			case types.Many2Many:
				query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s = ?`, fi.m2mTheirField.json,
					fi.m2mRelModel.tableName, fi.m2mOurField.json)
				var ids []int64
				rc.env.cr.Select(&ids, query, id)
				rc.env.cache.addEntry(rc.model, id, fieldName, ids)
			case types.Rev2One:
				relRC := rc.env.Pool(fi.relatedModelName).Search(rc.Model().Field(fi.reverseFK).Equals(id)).Fetch()
				var relID int64
				if len(relRC.ids) > 0 {
					relID = relRC.ids[0]
				}
				rc.env.cache.addEntry(rc.model, id, fieldName, relID)
			default:
				continue
			}
		}
	}
}

// Get returns the value of the given fieldName for the first record of this RecordCollection.
// It returns the type's zero value if the RecordCollection is empty.
func (rc RecordCollection) Get(fieldName string) interface{} {
	rSet := rc.Fetch()
	fi := rSet.model.fields.mustGet(fieldName)
	var res interface{}

	switch {
	case rSet.IsEmpty():
		res = reflect.Zero(fi.structField.Type).Interface()
	case fi.isComputedField() && !fi.isStored():
		fMap := make(FieldMap)
		rSet.computeFieldValues(&fMap, fi.json)
		res = fMap[fi.json]
	case fi.isRelatedField() && !fi.isStored():
		res = rSet.get(fi.relatedPath, false)
	default:
		// If value is not in cache we fetch the whole model to speed up later calls to Get,
		// except for the case of non stored relation fields, where we only load the requested field.
		all := !fi.fieldType.IsNonStoredRelationType()
		res = rSet.get(fieldName, all)
	}

	if res == nil {
		// res is nil if we do not have access rights on the field.
		// then return the field's type zero value
		return reflect.Zero(fi.structField.Type).Interface()
	}

	if fi.isRelationField() {
		switch r := res.(type) {
		case int64:
			res = newRecordCollection(rSet.Env(), fi.relatedModel.name)
			if r != 0 {
				res = res.(RecordCollection).withIds([]int64{r})
			}
		case []int64:
			res = newRecordCollection(rSet.Env(), fi.relatedModel.name).withIds(r)
		}
	}
	return res
}

// get returns the value of field for this RecordSet.
// It loads the cache if necessary before reading.
// If all is true, all fields of the model are loaded, otherwise only field.
func (rc RecordCollection) get(field string, all bool) interface{} {
	rSet := rc.Fetch()
	if !rSet.env.cache.checkIfInCache(rSet.model, []int64{rSet.ids[0]}, []string{field}) {
		if !all {
			rSet.Load(field)
		} else {
			rSet.Load()
		}
	}
	return rSet.env.cache.get(rSet.model, rSet.ids[0], field)
}

// Set sets field given by fieldName to the given value. If the RecordSet has several
// Records, all of them will be updated. Each call to Set makes an update query in the
// database. It panics if it is called on an empty RecordSet.
func (rc RecordCollection) Set(fieldName string, value interface{}) {
	fMap := make(FieldMap)
	fMap[fieldName] = value
	rc.Call("Write", fMap)
}

// First populates structPtr with a copy of the first Record of the RecordCollection.
// structPtr must a pointer to a struct.
func (rc RecordCollection) First(structPtr interface{}) {
	rSet := rc.Fetch()
	if err := checkStructPtr(structPtr); err != nil {
		log.Panic("Invalid structPtr given", "error", err, "model", rSet.ModelName(), "received", structPtr)
	}
	if rSet.IsEmpty() {
		return
	}
	typ := reflect.TypeOf(structPtr).Elem()
	fields := make([]string, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		fields[i] = typ.Field(i).Name
	}
	rSet.Load(fields...)
	fMap := rSet.env.cache.getRecord(rSet.ModelName(), rSet.ids[0])
	mapToStruct(rSet, structPtr, fMap)
}

// All fetches a copy of all records of the RecordCollection and populates structSlicePtr.
func (rc RecordCollection) All(structSlicePtr interface{}) {
	rSet := rc.Fetch()
	if err := checkStructSlicePtr(structSlicePtr); err != nil {
		log.Panic("Invalid structPtr given", "error", err, "model", rSet.ModelName(), "received", structSlicePtr)
	}
	val := reflect.ValueOf(structSlicePtr)
	// sspType is []*struct
	sspType := val.Type().Elem()
	// structType is struct
	structType := sspType.Elem().Elem()
	val.Elem().Set(reflect.MakeSlice(sspType, rSet.Len(), rSet.Len()))
	recs := rSet.Records()
	for i := 0; i < rSet.Len(); i++ {
		fMap := rSet.env.cache.getRecord(rSet.ModelName(), recs[i].ids[0])
		newStructPtr := reflect.New(structType).Interface()
		mapToStruct(rSet, newStructPtr, fMap)
		val.Elem().Index(i).Set(reflect.ValueOf(newStructPtr))
	}
}

// Aggregates returns the result of this RecordCollection query, which must by a grouped query.
func (rc RecordCollection) Aggregates(fieldNames ...FieldNamer) []GroupAggregateRow {
	if len(rc.query.groups) == 0 {
		log.Panic("Trying to get aggregates of a non-grouped query", "model", rc.model)
	}
	mustCheckModelPermission(rc.model, rc.env.uid, security.Read)
	rSet := rc.addRecordRuleConditions(rc.env.uid, security.Read)
	fields := filterOnAuthorizedFields(rSet.model, rSet.env.uid, convertToStringSlice(fieldNames), security.Read)
	subFields, rSet := rSet.substituteRelatedFields(fields)
	dbFields := filterOnDBFields(rSet.model, subFields, true)

	if len(rSet.query.orders) == 0 {
		rSet = rSet.OrderBy(rSet.query.groups...)
	}
	fieldsOperatorMap := rSet.fieldsGroupOperators(dbFields)
	sql, args := rSet.query.selectGroupQuery(fieldsOperatorMap)
	var res []GroupAggregateRow
	rows := dbQuery(rSet.env.cr.tx, sql, args...)
	defer rows.Close()

	for rows.Next() {
		vals := make(map[string]interface{})
		err := sqlx.MapScan(rows, vals)
		if err != nil {
			log.Panic(err.Error(), "model", rSet.ModelName(), "fields", fields)
		}
		cnt := vals["__count"].(int64)
		delete(vals, "__count")
		line := GroupAggregateRow{
			Values:    vals,
			Count:     int(cnt),
			Condition: getGroupCondition(rc.query.groups, vals),
		}
		res = append(res, line)
	}
	return res
}

// fieldsGroupOperators returns a map of fields to retrieve in a group by query.
// The returned map has a field as key, and sql aggregate function as value.
// it also includes 'field_count' for grouped fields
func (rc RecordCollection) fieldsGroupOperators(fields []string) map[string]string {
	groups := make(map[string]bool)
	for _, g := range rc.query.groups {
		groups[rc.model.JSONizeFieldName(g)] = true
	}
	res := make(map[string]string)
	for _, dbf := range fields {
		if groups[dbf] {
			res[dbf] = ""
			continue
		}
		fi := rc.model.getRelatedFieldInfo(dbf)
		if fi.fieldType != types.Float && fi.fieldType != types.Integer {
			continue
		}
		res[dbf] = fi.groupOperator
	}
	return res
}

// Records returns the slice of RecordCollection singletons that constitute this
// RecordCollection.
func (rc RecordCollection) Records() []RecordCollection {
	rSet := rc.Load()
	res := make([]RecordCollection, rSet.Len())
	for i, id := range rSet.Ids() {
		newRC := newRecordCollection(rSet.Env(), rSet.ModelName())
		res[i] = newRC.withIds([]int64{id})
	}
	return res
}

// EnsureOne panics if rc is not a singleton
func (rc RecordCollection) EnsureOne() {
	if rc.Len() != 1 {
		log.Panic("Expected singleton", "model", rc.ModelName(), "received", rc)
	}
}

// IsEmpty returns true if rc is an empty RecordCollection
func (rc RecordCollection) IsEmpty() bool {
	return rc.Len() == 0
}

// Len returns the number of records in this RecordCollection
func (rc RecordCollection) Len() int {
	rSet := rc.Fetch()
	return len(rSet.ids)
}

// Model returns the Model instance of this RecordCollection
func (rc RecordCollection) Model() *Model {
	return rc.model
}

// Union returns a new RecordCollection that is the union of this RecordCollection
// and the given `other` RecordCollection. The result is guaranteed to be a
// set of unique records.
func (rc RecordCollection) Union(other RecordCollection) RecordCollection {
	if rc.ModelName() != other.ModelName() {
		log.Panic("Unable to union RecordCollections of different models", "this", rc.ModelName(),
			"other", other.ModelName())
	}
	thisRC := rc.Fetch()
	otherRC := other.Fetch()
	idMap := make(map[int64]bool)
	for _, id := range thisRC.ids {
		idMap[id] = true
	}
	for _, id := range otherRC.ids {
		idMap[id] = true
	}
	ids := make([]int64, len(idMap))
	i := 0
	for id := range idMap {
		ids[i] = id
		i++
	}
	return newRecordCollection(rc.Env(), rc.ModelName()).withIds(ids)
}

// withIdMap returns a new RecordCollection pointing to the given ids.
// It overrides the current query with ("ID", "in", ids).
func (rc RecordCollection) withIds(ids []int64) RecordCollection {
	rSet := rc
	rSet.ids = ids
	rSet.fetched = true
	rSet.filtered = false
	if len(ids) > 0 {
		for _, id := range rSet.ids {
			rSet.env.cache.addEntry(rSet.model, id, "id", id)
		}
		rSet.query.cond = rc.Model().Field("ID").In(ids)
	}
	return rSet
}

var _ RecordSet = RecordCollection{}

// newRecordCollection returns a new empty RecordCollection in the
// given environment for the given modelName
func newRecordCollection(env Environment, modelName string) RecordCollection {
	mi := Registry.MustGet(modelName)
	rc := RecordCollection{
		model: mi,
		query: newQuery(),
		env:   &env,
		ids:   make([]int64, 0),
	}
	rc.query.recordSet = rc
	return rc
}
