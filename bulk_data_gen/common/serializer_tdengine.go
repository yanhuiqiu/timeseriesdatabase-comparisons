package common

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/pelletier/go-toml"
)

type serializerTDengine struct {
}

type TaosPoint struct {
	Name           string
	Type           string
	SourceType     int //1 for tag, 2 for value
	SourcePosition int //in the origin array
}

type Schemaconfig struct {
	Stname    string
	Suffix    string //suffix is selected from one of the tag/fields value in the input points,stname+suffix make a tablename
	Suffixpos int
	Tags      []TaosPoint //tag name and type  pairs set in the config file
	Fields    []TaosPoint //field name and type  pairs set in the config file
}
type TAOSConfig struct {
	schemaconfigs []Schemaconfig
}

var dbname string
var scalevar int64
var Schematree *toml.Tree
var IsSTableCreated sync.Map //a map to fast find whether the super table is created
var IsTableCreated sync.Map  //a map to fast find whether the table is created
//var Stabletoschema sync.Map      //a map to fast ge the schema

func NewSerializerTDengine(path string, dbn string, scaleVar int64) *serializerTDengine {
	var err error
	scalevar = scaleVar
	Schematree, err = TAOSNewConfig(path)
	if err != nil {
		fmt.Println("load taos config failed: %v", err)
		return nil
	}
	dbname = dbn

	return &serializerTDengine{}
}

// SerializeTDengineBulk writes Point data to the given writer, conforming to the
// TDengine wire protocol.
//
// This function writes output that looks like:
//Insert into db.tname Values (, , ,)\n
//
//
// TODO(rw): Speed up this function. The bulk of time is spent in strconv.
func (s *serializerTDengine) SerializePoint(w io.Writer, p *Point) error {
	err := TAOSCreateStable(w, p)
	if err != nil {
		return err
	}

	str := string(p.MeasurementName)
	//var schema *Schemaconfig

	out1, ok := IsSTableCreated.Load(str)
	if ok != true {
		//fmt.Println("can not load cfg ", str)
		return nil
	}
	//fmt.Println(out1)
	//fmt.Println(*out1)
	schema, ok1 := out1.(Schemaconfig)
	if ok1 != true {
		info := fmt.Errorf("can not restore from Schemaconfig %s ", schema)
		return info
	}

	var tbname string
	s2 := p.TagValues[schema.Suffixpos]
	tbindex, _ := strconv.ParseInt(string(s2[:]), 10, 64)
	tbname = str + "_" + strconv.FormatInt(tbindex, 10)
	buf := scratchBufPool.Get().([]byte)
	//buf = append(buf, "Insert into "...)
	head := fmt.Sprintf("%3d ", tbindex%scalevar)
	buf = append(buf, head...)
	buf = append(buf, tbname...)
	buf = append(buf, " using "...)
	buf = append(buf, str...)
	buf = append(buf, " tags("...)

	for i := 0; i < len(schema.Tags)-1; i++ {
		if schema.Tags[i].SourceType == 1 {
			v := p.TagValues[schema.Tags[i].SourcePosition]
			buf = fastFormatAppend(v, buf, false)
			buf = append(buf, ',')
		} else if schema.Tags[i].SourceType == 2 {
			v := p.FieldValues[schema.Tags[i].SourcePosition]
			buf = fastFormatAppend(v, buf, false)
			buf = append(buf, ',')
		} else {
			info := fmt.Sprintf("error type of tSourceType %d,pos %d", schema.Tags[i].SourceType, i)
			panic(info)
		}
	}
	if schema.Tags[len(schema.Tags)-1].SourceType == 1 {
		v := p.TagValues[schema.Tags[len(schema.Tags)-1].SourcePosition]
		buf = fastFormatAppend(v, buf, false)
		buf = append(buf, ") values("...)
	} else if schema.Tags[len(schema.Fields)-1].SourceType == 2 {
		v := p.FieldValues[schema.Tags[len(schema.Tags)-1].SourcePosition]
		buf = fastFormatAppend(v, buf, false)
		buf = append(buf, ") values("...)
	} else {
		info := fmt.Sprintf("error type of SourceType %d,pos %d", schema.Tags[len(schema.Tags)-1].SourceType, len(schema.Tags)-1)
		panic(info)
	}

	buf = fastFormatAppend(p.Timestamp.UTC().UnixNano()/1000000, buf, true)
	buf = append(buf, ',')
	for i := 0; i < len(schema.Fields)-1; i++ {
		if schema.Fields[i].SourceType == 1 {
			v := p.TagValues[schema.Fields[i].SourcePosition]
			buf = fastFormatAppend(v, buf, false)
			buf = append(buf, ',')
		} else if schema.Fields[i].SourceType == 2 {
			v := p.FieldValues[schema.Fields[i].SourcePosition]
			buf = fastFormatAppend(v, buf, false)
			buf = append(buf, ',')
		} else {
			info := fmt.Sprintf("error type of SourceType %d,pos %d", schema.Fields[i].SourceType, i)
			panic(info)
		}
	}
	if schema.Fields[len(schema.Fields)-1].SourceType == 1 {
		v := p.TagValues[schema.Fields[len(schema.Fields)-1].SourcePosition]
		buf = fastFormatAppend(v, buf, false)
		buf = append(buf, ")"...)
		buf = append(buf, '\n')
	} else if schema.Fields[len(schema.Fields)-1].SourceType == 2 {
		v := p.FieldValues[schema.Fields[len(schema.Fields)-1].SourcePosition]
		buf = fastFormatAppend(v, buf, false)
		buf = append(buf, ")"...)
		buf = append(buf, '\n')
	} else {
		info := fmt.Sprintf("error type of SourceType %d,pos %d", schema.Fields[len(schema.Fields)-1].SourceType, len(schema.Fields)-1)
		panic(info)
	}

	_, err = w.Write(buf)

	buf = buf[:0]
	scratchBufPool.Put(buf)

	return err
}

func (s *serializerTDengine) SerializeSize(w io.Writer, points int64, values int64) error {
	return serializeSizeInText(w, points, values)
}

func TAOSNewConfig(path string) (*toml.Tree, error) {
	var tree *toml.Tree
	var err error

	tree, err = toml.LoadFile(path)

	if err != nil {
		return nil, fmt.Errorf("config loading failed: %v", err)
	}
	return tree, nil
}

func LoadSchema(measurement string, tree *toml.Tree) (*TAOSConfig, error) {
	obj := tree.ToMap()[measurement]
	//fmt.Println(obj)
	if obj == nil {
		return nil, nil
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("config marshall failed: %v", err)
	}
	config := TAOSConfig{}
	err = json.Unmarshal(b, &config.schemaconfigs)
	if err != nil {
		return nil, fmt.Errorf("config unmarshall failed: %v", err)
	}
	//var str string = string(b[:])

	return &config, nil
}

func TAOSCreateStable(w io.Writer, p *Point) error {
	stablename := string(p.MeasurementName[:])
	out, _ := IsSTableCreated.Load(stablename)

	if out != nil {

		return nil
	}

	//IsSTableCreated.Store(stablename,true)
	s1, err := LoadSchema(stablename, Schematree)

	if err != nil || s1 == nil {
		return err
	}

	taosconfig := s1.schemaconfigs[0]
	//var tbname string

	//var fndflag int = 0

	// find the tag
	for i := 0; i < len(taosconfig.Tags); i++ {
		tn := taosconfig.Tags[i].Name
		found := 0
		for j := 0; j < len(p.TagKeys); j++ {

			if tn == string(p.TagKeys[j][:]) {
				taosconfig.Tags[i].SourceType = 1
				taosconfig.Tags[i].SourcePosition = j
				found = 1
				break
			}
		}
		if found == 0 {
			for j := 0; j < len(p.FieldKeys); j++ {

				if tn == string(p.FieldKeys[j][:]) {
					taosconfig.Tags[i].SourceType = 2
					taosconfig.Tags[i].SourcePosition = j
					found = 1
					break
				}
			}
		}
		if found == 0 {
			info := fmt.Sprintf("Config error, cannot find tagname %s in the point", tn)
			panic(info)
		}
	}

	//find the fields
	for i := 0; i < len(taosconfig.Fields); i++ {
		tn := taosconfig.Fields[i].Name
		found := 0
		for j := 0; j < len(p.TagKeys); j++ {

			if tn == string(p.TagKeys[j][:]) {
				taosconfig.Fields[i].SourceType = 1
				taosconfig.Fields[i].SourcePosition = j
				found = 1
				break
			}
		}
		if found == 0 {
			for j := 0; j < len(p.FieldKeys); j++ {

				if tn == string(p.FieldKeys[j][:]) {
					taosconfig.Fields[i].SourceType = 2
					taosconfig.Fields[i].SourcePosition = j
					found = 1
					break
				}
			}
		}
		if found == 0 {
			info := fmt.Sprintf("Config error, cannot find fieldname %s in the point", tn)
			panic(info)
		}
	}

	var foundtbn int = 0
	//find the table name suffix
	tnsuffix := taosconfig.Suffix
	for i := 0; i < len(p.TagKeys); i++ {
		tagkey := p.TagKeys[i]
		tagstr := string(tagkey[:])
		if tnsuffix == tagstr {
			taosconfig.Suffixpos = i
			foundtbn = 1
			break
		}
	}

	if foundtbn == 0 {
		panic("config error, can not find table suffix ")
	}
	//IsTableCreated.Store(tbname,true)

	//store the schema
	IsSTableCreated.Store(stablename, taosconfig)

	// assemble the create super table command line
	buf := scratchBufPool.Get().([]byte)
	s := fmt.Sprintf("create table %s (ts timestamp ", stablename)
	buf = append(buf, s...)
	for i := 0; i < len(taosconfig.Fields); i++ {
		buf = append(buf, ", f_"+taosconfig.Fields[i].Name+" "+taosconfig.Fields[i].Type...)
	}
	buf = append(buf, ") tags ("...)
	for i := 0; i < len(taosconfig.Tags); i++ {
		if i == 0 {
			buf = append(buf, "t_"+taosconfig.Tags[i].Name+" "+taosconfig.Tags[i].Type...)
		} else {
			buf = append(buf, ", t_"+taosconfig.Tags[i].Name+" "+taosconfig.Tags[i].Type...)
		}
	}
	buf = append(buf, ");\n"...)
	_, _ = w.Write(buf)
	buf = buf[:0]
	scratchBufPool.Put(buf)
	return nil
}
