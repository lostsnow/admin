package admin

import (
	"errors"
	"fmt"
	"github.com/extemporalgenome/slug"
	"github.com/oal/admin/fields"
	"io"
	"reflect"
)

// NamedModel requires an AdminName method to be present, to override the model's displayed name in the admin panel.
type NamedModel interface {
	AdminName() string
}

type modelGroup struct {
	admin  *Admin
	Name   string
	slug   string
	Models []*model
}

// RegisterModel adds a model to a model group.
func (g *modelGroup) RegisterModel(mdl interface{}) error {
	modelType := reflect.TypeOf(mdl)
	ind := reflect.Indirect(reflect.ValueOf(mdl))

	name := typeToName(modelType)
	tableName := typeToTableName(modelType, g.admin.NameTransform)

	if named, ok := mdl.(NamedModel); ok {
		name = named.AdminName()
	}

	newModel := model{
		Name:      name,
		Slug:      slug.SlugAscii(name),
		tableName: tableName,
		fields:    []fields.Field{},

		fieldNames:        []string{},
		listFields:        []fields.Field{},
		searchableColumns: []string{},
	}

	// Set as registered so it can be used as a ForeignKey from other models
	if _, ok := g.admin.registeredFKs[modelType]; !ok {
		g.admin.registeredFKs[modelType] = &newModel
	}

	// Check if any fields previously registered is missing this model as a foreign key
	for field, modelType := range g.admin.missingFKs {
		if modelType != modelType {
			continue
		}

		field.ModelSlug = newModel.Slug
		delete(g.admin.missingFKs, field)
	}

	// Loop over struct fields and set up fields
	for i := 0; i < ind.NumField(); i++ {
		refl := modelType.Elem().Field(i)
		fieldType := refl.Type
		kind := fieldType.Kind()

		// Parse key=val / key options from struct tag, used for configuration later
		tag := refl.Tag.Get("admin")
		if tag == "-" {
			if i == 0 {
				return errors.New("First column (id) can't be skipped.")
			}
			continue
		}
		tagMap, err := parseTag(tag)
		if err != nil {
			panic(err)
		}

		// ID (i == 0) is always shown
		if i == 0 {
			tagMap["list"] = ""
		}

		override, _ := tagMap["field"]
		field := makeField(kind, override)

		// Foreign keys need some additional data added to them
		if fkField, ok := field.(*fields.ForeignKeyField); ok {
			// If column is shown in list view, and a field in related model is set to be listed
			if listField, ok := tagMap["list"]; ok && len(listField) != 0 {
				fkField.TableName = typeToTableName(refl.Type, g.admin.NameTransform)
				if g.admin.NameTransform != nil {
					listField = g.admin.NameTransform(listField)
				}
				fkField.ListColumn = listField
			}

			// We also need the field to know what model it's related to
			if regModel, ok := g.admin.registeredFKs[fieldType]; ok {
				fkField.ModelSlug = regModel.Slug
			} else {
				g.admin.missingFKs[field.(*fields.ForeignKeyField)] = refl.Type
			}
			field = fkField
		}

		// Expect pointers to be foreign keys and foreign keys to have the form Field[Id]
		fieldName := refl.Name
		if kind == reflect.Ptr {
			fieldName += "Id"
		}

		// Transform struct keys to DB column names if needed
		var tableField string
		if g.admin.NameTransform != nil {
			tableField = g.admin.NameTransform(fieldName)
		} else {
			tableField = refl.Name
		}

		field.Attrs().Name = fieldName
		field.Attrs().ColumnName = tableField
		applyFieldTags(&newModel, field, tagMap)

		newModel.fields = append(newModel.fields, field)
		newModel.fieldNames = append(newModel.fieldNames, fieldName)
	}

	g.admin.models[newModel.Slug] = &newModel
	g.Models = append(g.Models, &newModel)

	fmt.Println("Registered", newModel.Name)
	return nil
}

func makeField(kind reflect.Kind, override string) fields.Field {
	// First, check if we want to override a field, otherwise use one of the defaults
	var field fields.Field
	if customField := fields.GetCustom(override); customField != nil {
		// Create field
		customType := reflect.ValueOf(customField).Elem().Type()
		newField := reflect.New(customType)

		// Create BaseField in Field
		baseField := newField.Elem().Field(0)
		baseField.Set(reflect.ValueOf(&fields.BaseField{}))

		field = newField.Interface().(fields.Field)
	} else {
		switch kind {
		case reflect.String:
			field = &fields.TextField{BaseField: &fields.BaseField{}}
		case reflect.Int, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			field = &fields.IntField{BaseField: &fields.BaseField{}}
		case reflect.Float32, reflect.Float64:
			field = &fields.FloatField{BaseField: &fields.BaseField{}}
		case reflect.Bool:
			field = &fields.BooleanField{BaseField: &fields.BaseField{}}
		case reflect.Struct:
			field = &fields.TimeField{BaseField: &fields.BaseField{}}
		case reflect.Ptr:
			field = &fields.ForeignKeyField{BaseField: &fields.BaseField{}}
		default:
			fmt.Println("Unknown field type")
			field = &fields.TextField{BaseField: &fields.BaseField{}}
		}
	}

	return field
}

func applyFieldTags(mdl *model, field fields.Field, tagMap map[string]string) {
	// Read relevant config options from the tagMap
	err := field.Configure(tagMap)
	if err != nil {
		panic(err)
	}

	if label, ok := tagMap["label"]; ok {
		field.Attrs().Label = label
	} else {
		field.Attrs().Label = field.Attrs().Name
	}

	if _, ok := tagMap["optional"]; ok {
		field.Attrs().Optional = true
	}

	if _, ok := tagMap["list"]; ok {
		field.Attrs().List = true
		mdl.listFields = append(mdl.listFields, field)
	}

	if _, ok := tagMap["search"]; ok {
		field.Attrs().Searchable = true
		mdl.searchableColumns = append(mdl.searchableColumns, field.Attrs().ColumnName)
	}

	if val, ok := tagMap["default"]; ok {
		field.Attrs().DefaultValue = val
	}

	if width, ok := tagMap["width"]; ok {
		i, err := parseInt(width)
		if err != nil {
			panic(err)
		}
		field.Attrs().Width = i
	}
}

type model struct {
	Name      string
	Slug      string
	fields    []fields.Field
	tableName string

	fieldNames        []string
	listFields        []fields.Field
	searchableColumns []string
}

func (m *model) renderForm(w io.Writer, data map[string]interface{}, defaults bool, errors map[string]string) {
	var val interface{}
	var ok bool
	activeCol := 0
	for _, fieldName := range m.fieldNames[1:] {
		field := m.fieldByName(fieldName)
		val, ok = data[fieldName]
		if !ok && defaults {
			val = field.Attrs().DefaultValue
		}

		// Error text displayed below field, if any
		var err string
		if errors != nil {
			err = errors[fieldName]
		}

		field.Render(w, val, err, activeCol%12 == 0)
		activeCol += field.Attrs().Width
	}
}

func (m *model) fieldByName(name string) fields.Field {
	for _, field := range m.fields {
		if field.Attrs().Name == name {
			return field
		}
	}
	return nil
}