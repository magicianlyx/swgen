package swgen

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	refDefinitionPrefix = "#/definitions/"
)

var (
	typeOfJSONRawMsg      = reflect.TypeOf((*json.RawMessage)(nil)).Elem()
	typeOfTime            = reflect.TypeOf((*time.Time)(nil)).Elem()
	typeOfTextUnmarshaler = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
)

// IParameter allows to return custom parameters
type IParameter interface {
	SwgenParameter() (name string, params []ParamObj, err error)
}

// IDefinition allows to return custom definitions
type IDefinition interface {
	SwgenDefinition() (typeName string, typeDef SchemaObj, err error)
}

func (g *Generator) addDefinition(t reflect.Type, typeDef *SchemaObj) {
	if typeDef.TypeName == "" {
		return // there should be no anonymous definitions in Swagger JSON
	}
	if _, ok := g.definitions[t]; ok { // skip existing definition
		return
	}

	if _, ok := g.definitionAdded[typeDef.TypeName]; ok { // process duplicate TypeName
		var typeName string
		typeIndex := 2
		for {
			typeName = fmt.Sprintf("%sType%d", typeDef.TypeName, typeIndex)
			if _, ok := g.definitionAdded[typeName]; !ok {
				break
			}
			typeIndex++
		}

		typeDef.TypeName = typeName
		if typeDef.Ref != "" {
			typeDef.Ref = refDefinitionPrefix + typeDef.TypeName
		}
	}
	g.definitionAdded[typeDef.TypeName] = true
	g.definitions[t] = *typeDef
}

func (g *Generator) defExists(t reflect.Type) (b bool) {
	_, b = g.definitions[t]
	return b
}

func (g *Generator) addToDefQueue(t reflect.Type) {
	g.defQueue[t] = struct{}{}
}

func (g *Generator) defInQueue(t reflect.Type) (found bool) {
	_, found = g.defQueue[t]
	return
}

func (g *Generator) getDefinition(t reflect.Type) (typeDef SchemaObj, found bool) {
	typeDef, found = g.definitions[t]
	if !found && t.Kind() == reflect.Ptr {
		typeDef, found = g.definitions[t.Elem()]
	}
	return
}

func (g *Generator) deleteDefinition(t reflect.Type) {
	delete(g.definitions, t)
}

//
// Parse swagger schema object
// see http://swagger.io/specification/#schemaObject
//

// ResetDefinitions will remove all exists definitions and init again
func (g *Generator) ResetDefinitions() {
	g.definitions = make(defMap)
	g.definitionAdded = make(map[string]bool)
	g.defQueue = make(map[reflect.Type]struct{})
}

// ResetDefinitions will remove all exists definitions and init again
func ResetDefinitions() {
	gen.ResetDefinitions()
}

// ParseDefinition create a DefObj from input object, it should be a non-nil pointer to anything
// it reuse schema/json tag for property name.
func (g *Generator) ParseDefinition(i interface{}) (schema SchemaObj, err error) {
	var (
		typeName string
		typeDef  SchemaObj
		t        = reflect.TypeOf(i)
		v        = reflect.ValueOf(i)
	)

	if mappedTo, ok := g.getMappedType(t); ok {
		typeName = t.Name()
		t = reflect.TypeOf(mappedTo)
		v = reflect.ValueOf(mappedTo)
	}

	if definition, ok := i.(IDefinition); ok {
		typeName, typeDef, err = definition.SwgenDefinition()
		if err != nil {
			return typeDef, err
		}
		if typeName == "" {
			typeName = t.Name()
		}
		typeDef.TypeName = typeName
		if def, ok := g.getDefinition(t); ok {
			return SchemaObj{Ref: refDefinitionPrefix + def.TypeName, TypeName: def.TypeName}, nil
		}
		defer g.parseDefInQueue()
		if g.reflectGoTypes {
			typeDef.GoType = goType(t)
		}
		g.addDefinition(t, &typeDef)

		return SchemaObj{Ref: refDefinitionPrefix + typeDef.TypeName, TypeName: typeDef.TypeName}, nil
	}

	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	switch t.Kind() {
	case reflect.Struct:
		if typeDef, found := g.getDefinition(t); found {
			return typeDef.Export(), nil
		}

		typeDef = *NewSchemaObj("object", ReflectTypeReliableName(t))
		typeDef.Properties = g.parseDefinitionProperties(v, &typeDef)
		if typeDef.TypeName == "" {
			typeDef.TypeName = typeName
		}

		// if len(typeDef.Properties) == 0 {
		//	typeDef.Ref = ""
		// }
	case reflect.Slice, reflect.Array:
		elemType := t.Elem()
		if elemType.Kind() == reflect.Ptr {
			elemType = elemType.Elem()
		}

		if typeDef, found := g.getDefinition(t); found {
			return typeDef.Export(), nil
		}

		var itemSchema SchemaObj
		if elemType.Kind() != reflect.Struct || (elemType.Kind() == reflect.Struct && elemType.Name() != "") {
			itemSchema = g.genSchemaForType(elemType)
		} else {
			itemSchema = *NewSchemaObj("object", elemType.Name())
			itemSchema.Properties = g.parseDefinitionProperties(v.Elem(), &itemSchema)
		}

		typeDef = *NewSchemaObj("array", t.Name())
		typeDef.Items = &itemSchema
		if typeDef.TypeName == "" {
			typeDef.TypeName = typeName
		}
	case reflect.Map:
		elemType := t.Elem()
		if elemType.Kind() == reflect.Ptr {
			elemType = elemType.Elem()
		}

		if typeDef, found := g.getDefinition(t); found {
			return typeDef.Export(), nil
		}

		typeDef = *NewSchemaObj("object", t.Name())
		itemDef := g.genSchemaForType(elemType)
		typeDef.AdditionalProperties = &itemDef
		if typeDef.TypeName == "" {
			typeDef.TypeName = typeName
		}
	default:
		typeDef = g.genSchemaForType(t)
		typeDef.TypeName = typeDef.Type
		return typeDef, nil
	}

	defer g.parseDefInQueue()

	if g.reflectGoTypes {
		typeDef.GoType = goType(t)
	}

	if typeDef.TypeName != "" { // non-anonymous types should be added to definitions map and returned "in-place" as references
		g.addDefinition(t, &typeDef)
		return typeDef.Export(), nil
	}
	return typeDef, nil // anonymous types are not added to definitions map; instead, they are returned "in-place" in full form
}

func goType(t reflect.Type) (s string) {
	s = t.Name()
	pkgPath := t.PkgPath()
	if pkgPath != "" {
		pos := strings.Index(pkgPath, "/vendor/")
		if pos != -1 {
			pkgPath = pkgPath[pos+8:]
		}
		s = pkgPath + "." + s
	}

	ts := t.String()
	typeRef := s

	pos := strings.LastIndex(typeRef, "/")
	if pos != -1 {
		typeRef = typeRef[pos+1:]
	}

	if typeRef != ts {
		s = s + "::" + t.String()
	}

	switch t.Kind() {
	case reflect.Slice:
		return "[]" + goType(t.Elem())
	case reflect.Ptr:
		return "*" + goType(t.Elem())
	case reflect.Map:
		return "map[" + goType(t.Key()) + "]" + goType(t.Elem())
	}

	return
}

func (g *Generator) parseDefinitionProperties(v reflect.Value, parent *SchemaObj) map[string]SchemaObj {
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	t := v.Type()
	properties := make(map[string]SchemaObj, t.NumField())
	if g.reflectGoTypes && parent.GoPropertyNames == nil {
		parent.GoPropertyNames = make(map[string]string, t.NumField())
		parent.GoPropertyTypes = make(map[string]string, t.NumField())
	}

	for i := 0; i < t.NumField(); i = i + 1 {
		field := t.Field(i)

		// we can't access the value of un-exportable field
		if field.PkgPath != "" {
			continue
		}

		if field.Anonymous {
			fieldProperties := g.parseDefinitionProperties(v.Field(i), parent)
			for propertyName, property := range fieldProperties {
				properties[propertyName] = property
			}
			continue
		}

		// don't check if it's omitted
		var tag string
		if tag = field.Tag.Get("json"); tag == "-" || tag == "" {
			continue
		}

		propName := strings.Split(tag, ",")[0]
		var (
			obj SchemaObj
		)

		if dataType := field.Tag.Get("swgen_type"); dataType != "" {
			obj = SchemaFromCommonName(commonName(dataType))
		} else {
			if field.Type.Kind() == reflect.Interface && v.Field(i).Elem().IsValid() {
				obj = g.genSchemaForType(v.Field(i).Elem().Type())
			} else {
				obj = g.genSchemaForType(field.Type)
			}
		}

		if defaultTag := field.Tag.Get("default"); defaultTag != "" {
			if defaultValue, err := g.caseDefaultValue(field.Type, defaultTag); err == nil {
				obj.Default = defaultValue
			}
		}
		if g.reflectGoTypes {
			if obj.Ref == "" {
				obj.GoType = goType(field.Type)
			}
			parent.GoPropertyNames[propName] = field.Name
			parent.GoPropertyTypes[propName] = goType(field.Type)
		}

		properties[propName] = obj
	}

	return properties
}

func (g *Generator) caseDefaultValue(t reflect.Type, defaultValue string) (interface{}, error) {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	kind := t.Kind()

	switch kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.ParseInt(defaultValue, 10, 64)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.ParseUint(defaultValue, 10, 64)
	case reflect.Float32, reflect.Float64:
		return strconv.ParseFloat(defaultValue, 64)
	case reflect.String:
		return defaultValue, nil
	case reflect.Bool:
		return strconv.ParseBool(defaultValue)
	default:
		instance := reflect.New(t).Interface()
		if err := json.Unmarshal([]byte(defaultValue), instance); err != nil {
			return nil, err
		}
		return reflect.Indirect(reflect.ValueOf(instance)).Interface(), nil
	}
}

// ParseDefinition create a DefObj from input object, it should be a pointer to a struct,
// it reuse schema/json tag for property name.
func ParseDefinition(i interface{}) (typeDef SchemaObj, err error) {
	return gen.ParseDefinition(i)
}

func (g *Generator) parseDefInQueue() {
	if len(g.defQueue) == 0 {
		return
	}

	for t := range g.defQueue {
		g.ParseDefinition(reflect.Zero(t).Interface())
	}
}

func (g *Generator) genSchemaForType(t reflect.Type) SchemaObj {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	smObj := SchemaObj{TypeName: t.Name()}

	switch t.Kind() {
	case reflect.Bool:
		smObj = SchemaFromCommonName(CommonNameBoolean)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Uint, reflect.Uint8, reflect.Uint16:
		smObj = SchemaFromCommonName(CommonNameInteger)
	case reflect.Int64, reflect.Uint32, reflect.Uint64:
		smObj = SchemaFromCommonName(CommonNameLong)
	case reflect.Float32:
		smObj = SchemaFromCommonName(CommonNameFloat)
	case reflect.Float64:
		smObj = SchemaFromCommonName(CommonNameDouble)
	case reflect.String:
		smObj = SchemaFromCommonName(CommonNameString)
	case reflect.Array, reflect.Slice:
		if t != typeOfJSONRawMsg {
			smObj.Type = "array"
			itemSchema := g.genSchemaForType(t.Elem())
			smObj.Items = &itemSchema
		}
	case reflect.Map:
		smObj.Type = "object"
		itemSchema := g.genSchemaForType(t.Elem())
		smObj.AdditionalProperties = &itemSchema
	case reflect.Struct:
		switch {
		case t == typeOfTime:
			smObj = SchemaFromCommonName(CommonNameDateTime)
		case reflect.PtrTo(t).Implements(typeOfTextUnmarshaler):
			smObj.Type = "string"
		default:
			name := ReflectTypeReliableName(t)
			smObj.Ref = refDefinitionPrefix + name
			if !g.defExists(t) || !g.defInQueue(t) {
				g.addToDefQueue(t)
			}
		}
	case reflect.Interface:
		if t.NumMethod() > 0 {
			panic("Non-empty interface is not supported: " + t.String())
		}
	default:
		panic(fmt.Sprintf("type %s is not supported: %s", t.Kind(), t.String()))
	}

	if g.reflectGoTypes && smObj.Ref == "" {
		smObj.GoType = goType(t)
	}

	return smObj
}

//
// Parse struct to swagger parameter object of operation object
// see http://swagger.io/specification/#parameterObject
//

// ParseParameter parse input struct to swagger parameter object
func (g *Generator) ParseParameter(i interface{}) (name string, params []ParamObj, err error) {
	if param, ok := i.(IParameter); ok {
		return param.SwgenParameter()
	}

	v := reflect.ValueOf(i)

	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	if v.Kind() != reflect.Struct {
		err = errors.New("Generator.ParseParameter() failed: parameters must be a struct")
		return
	}

	t := v.Type()

	if mappedTo, ok := g.getMappedType(t); ok {
		return g.ParseParameter(mappedTo)
	}

	name = t.Name()
	params = []ParamObj{}

	ForEachField(i, func(field reflect.StructField, value interface{}) bool {

		// // we can't access the value of un-exportable or anonymous fields
		// if field.PkgPath != "" || field.Anonymous {
		// 	continue
		// }

		// don't check if it's omitted
		var nameTag string

		var inPath bool
		if nameTag = field.Tag.Get("query"); nameTag == "-" || nameTag == "" {

			if nameTag = field.Tag.Get("form"); nameTag == "-" || nameTag == "" {

				if nameTag = field.Tag.Get("schema"); nameTag == "-" || nameTag == "" {
					inPath = true
					if nameTag = field.Tag.Get("path"); nameTag == "-" || nameTag == "" {
						return true
					}
				}
			}
		}

		paramName := strings.Split(nameTag, ",")[0]
		param := ParamObj{}
		if g.reflectGoTypes {
			param.AddExtendedField("x-go-name", field.Name)
			param.AddExtendedField("x-go-type", goType(field.Type))
		}

		param.Name = paramName

		if e, isEnumer := reflect.Zero(field.Type).Interface().(enumer); isEnumer {
			param.Enum.Enum, param.Enum.EnumNames = e.GetEnumSlices()
		}

		if descTag := field.Tag.Get("description"); descTag != "-" && descTag != "" {
			param.Description = descTag
		}

		binding := field.Tag.Get("binding")
		bindings := strings.Split(binding, ";")

		if len(binding) != 0 && Contains(bindings, "required") {
			param.Required = true
		} else {
			param.Required = false
		}

		if inTag := field.Tag.Get("in"); inTag != "-" && inTag != "" {
			param.In = inTag // todo: validate IN value
		} else if inPath {
			param.In = "path"
		} else {
			param.In = "query"
		}

		var schema SchemaObj
		if swGenType := field.Tag.Get("swgen_type"); swGenType != "" {
			schema = SchemaFromCommonName(commonName(swGenType))
		} else {
			if mappedTo, ok := g.getMappedType(field.Type); ok {
				schema = g.genSchemaForType(reflect.TypeOf(mappedTo))
			} else {
				schema = g.genSchemaForType(field.Type)
			}
		}

		if schema.Type == "" {
			panic("dont support struct " + v.Type().Name() + " in property " + field.Name + " of parameter struct")
		}

		param.Type = schema.Type
		param.Format = schema.Format

		if schema.Type == "array" && schema.Items != nil {
			if schema.Items.Ref != "" || schema.Items.Type == "array" {
				panic("dont support array of struct or nested array in parameter")
			}

			param.Items = &ParamItemObj{
				Type:   schema.Items.Type,
				Format: schema.Items.Format,
			}
			param.CollectionFormat = "multi" // default for now
		}

		params = append(params, param)
		return true
	})
	// for i := 0; i < t.NumField(); i = i + 1 {
	// 	field := t.Field(i)
	// 	// we can't access the value of un-exportable or anonymous fields
	// 	if field.PkgPath != "" || field.Anonymous {
	// 		continue
	// 	}
	//
	// 	// don't check if it's omitted
	// 	var nameTag string
	//
	// 	var inPath bool
	// 	if nameTag = field.Tag.Get("schema"); nameTag == "-" || nameTag == "" {
	// 		inPath = true
	// 		if nameTag = field.Tag.Get("path"); nameTag == "-" || nameTag == "" {
	// 			continue
	// 		}
	// 	}
	//
	// 	paramName := strings.Split(nameTag, ",")[0]
	// 	param := ParamObj{}
	// 	if g.reflectGoTypes {
	// 		param.AddExtendedField("x-go-name", field.Name)
	// 		param.AddExtendedField("x-go-type", goType(field.Type))
	// 	}
	//
	// 	param.Name = paramName
	//
	// 	if e, isEnumer := reflect.Zero(field.Type).Interface().(enumer); isEnumer {
	// 		param.Enum.Enum, param.Enum.EnumNames = e.GetEnumSlices()
	// 	}
	//
	// 	if descTag := field.Tag.Get("description"); descTag != "-" && descTag != "" {
	// 		param.Description = descTag
	// 	}
	//
	// 	if reqTag := field.Tag.Get("required"); reqTag == "-" || reqTag == "false" {
	// 		param.Required = false
	// 	} else {
	// 		param.Required = true
	// 	}
	//
	// 	if inTag := field.Tag.Get("in"); inTag != "-" && inTag != "" {
	// 		param.In = inTag // todo: validate IN value
	// 	} else if inPath {
	// 		param.In = "path"
	// 	} else {
	// 		param.In = "query"
	// 	}
	//
	// 	var schema SchemaObj
	// 	if swGenType := field.Tag.Get("swgen_type"); swGenType != "" {
	// 		schema = SchemaFromCommonName(commonName(swGenType))
	// 	} else {
	// 		if mappedTo, ok := g.getMappedType(field.Type); ok {
	// 			schema = g.genSchemaForType(reflect.TypeOf(mappedTo))
	// 		} else {
	// 			schema = g.genSchemaForType(field.Type)
	// 		}
	// 	}
	//
	// 	if schema.Type == "" {
	// 		panic("dont support struct " + v.Type().Name() + " in property " + field.Name + " of parameter struct")
	// 	}
	//
	// 	param.Type = schema.Type
	// 	param.Format = schema.Format
	//
	// 	if schema.Type == "array" && schema.Items != nil {
	// 		if schema.Items.Ref != "" || schema.Items.Type == "array" {
	// 			panic("dont support array of struct or nested array in parameter")
	// 		}
	//
	// 		param.Items = &ParamItemObj{
	// 			Type:   schema.Items.Type,
	// 			Format: schema.Items.Format,
	// 		}
	// 		param.CollectionFormat = "multi" // default for now
	// 	}
	//
	// 	params = append(params, param)
	// }

	return
}

// ParseParameter parse input struct to swagger parameter object
func ParseParameter(i interface{}) (name string, params []ParamObj, err error) {
	return gen.ParseParameter(i)
}

func ForEachField(o interface{}, f func(field reflect.StructField, value interface{}) bool) {
	if o == nil {
		return
	}

	v := reflect.ValueOf(o)

	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	t := v.Type()

	for i := 0; i < v.NumField(); i++ {
		tf := t.Field(i)
		vf := v.Field(i)

		// 不包括私有字段（私有字段不在递归）
		if !IsCapitalHeader(tf.Name) {
			continue
		}

		if tf.Type.Kind() == reflect.Ptr {
			if tf.Type.Elem().Kind() == reflect.Struct {
				vok := reflect.New(tf.Type.Elem()).Interface()
				ForEachField(vok, f)
			}
		} else if tf.Type.Kind() == reflect.Struct {
			ForEachField(vf.Interface(), f)
		}

		success := f(tf, vf.Interface())
		if !success {
			return
		}
	}
}

// 是否为大写开头
func IsCapitalHeader(s string) bool {
	if len(s) == 0 {
		return false
	}
	head := s[:1]
	t := []rune(head)
	if t[0] >= 65 && t[0] < 91 {
		return true
	} else {
		return false
	}
}

func Contains(list []string, s string) bool {
	for _, t := range list {
		if t == s {
			return true
		}
	}
	return false
}

// ResetPaths remove all current paths
func (g *Generator) ResetPaths() {
	g.paths = make(map[string]PathItem)
}

// ResetPaths remove all current paths
func ResetPaths() {
	gen.ResetPaths()
}

var regexFindPathParameter = regexp.MustCompile(`\{([^}:]+)(:[^\/]+)?(?:\})`)

// SetPathItem register path item with some information and input, output
func (g *Generator) SetPathItem(info PathItemInfo, params interface{}, body interface{}, response interface{}) error {
	var (
		item  PathItem
		found bool
	)

	pathParametersSubmatches := regexFindPathParameter.FindAllStringSubmatch(info.Path, -1)
	if len(pathParametersSubmatches) > 0 {
		for _, submatch := range pathParametersSubmatches {
			if submatch[2] != "" { // Remove gorilla.Mux-style regexp in path
				info.Path = strings.Replace(info.Path, submatch[0], "{"+submatch[1]+"}", 1)
			}
		}
	}

	item, found = g.paths[info.Path]

	if found && item.HasMethod(info.Method) {
		return nil
	}

	if !found {
		item = PathItem{}
	}

	operationObj := &OperationObj{}
	operationObj.Summary = info.Title
	operationObj.Description = info.Description
	operationObj.Deprecated = info.Deprecated
	operationObj.additionalData = info.additionalData
	if info.Tag != "" {
		operationObj.Tags = []string{info.Tag}
	}

	operationObj.Security = make(map[string][]string)
	if len(info.Security) > 0 {
		for _, sec := range info.Security {
			if _, ok := g.doc.SecurityDefinitions[sec]; ok {
				operationObj.Security[sec] = []string{}
			} else {
				return errors.New("Undefined security definition: " + sec)
			}
		}
	}

	if len(info.SecurityOAuth2) > 0 {
		for sec, scopes := range info.SecurityOAuth2 {
			if _, ok := g.doc.SecurityDefinitions[sec]; ok {
				operationObj.Security[sec] = scopes
			} else {
				return errors.New("Undefined security definition: " + sec)
			}
		}
	}

	if params != nil {
		if g.reflectGoTypes {
			operationObj.AddExtendedField("x-request-go-type", goType(reflect.TypeOf(params)))
		}

		if _, params, err := g.ParseParameter(params); err == nil {
			operationObj.Parameters = params
		} else {
			return err
		}
	}

	operationObj.Responses = g.parseResponseObject(response)

	if body != nil {
		if g.reflectGoTypes {
			operationObj.AddExtendedField("x-request-go-type", goType(reflect.TypeOf(body)))
		}

		typeDef, err := g.ParseDefinition(body)

		if err != nil {
			return err
		}

		if !typeDef.isEmpty() {
			param := ParamObj{
				Name:     "body",
				In:       "body",
				Required: true,
				Schema:   &typeDef,
			}

			if operationObj.Parameters == nil {
				operationObj.Parameters = make([]ParamObj, 0, 1)
			}

			operationObj.Parameters = append(operationObj.Parameters, param)
		} else {
			g.deleteDefinition(reflect.TypeOf(body))
		}
	}

	switch strings.ToUpper(info.Method) {
	case "GET":
		item.Get = operationObj
	case "POST":
		item.Post = operationObj
	case "PUT":
		item.Put = operationObj
	case "DELETE":
		item.Delete = operationObj
	case "OPTIONS":
		item.Options = operationObj
	case "HEAD":
		item.Head = operationObj
	case "PATCH":
		item.Patch = operationObj
	}

	g.paths[info.Path] = item

	return nil
}

// SetPathItem register path item with some information and input, output
func SetPathItem(info PathItemInfo, params interface{}, body interface{}, response interface{}) error {
	return gen.SetPathItem(info, params, body, response)
}

func (g *Generator) parseResponseObject(responseObj interface{}) (res Responses) {
	res = make(Responses)

	if responseObj != nil {
		schema, err := g.ParseDefinition(responseObj)
		if err != nil {
			panic(fmt.Sprintf("could not create schema object for response %v", responseObj))
		}
		// since we only response json object
		// so, type of response object is always object
		res["200"] = ResponseObj{
			Description: "request success",
			Schema:      &schema,
		}
	} else {
		res["200"] = ResponseObj{
			Description: "request success",
			Schema:      &SchemaObj{Type: "null"},
		}
	}

	return res
}
