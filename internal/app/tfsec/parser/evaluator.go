package parser

import (
	"reflect"

	"github.com/tfsec/tfsec/internal/app/tfsec/block"

	"github.com/tfsec/tfsec/internal/app/tfsec/metrics"

	"github.com/hashicorp/hcl/v2"
	"github.com/tfsec/tfsec/internal/app/tfsec/debug"
	"github.com/zclconf/go-cty/cty"
)

const maxContextIterations = 32

type visitedModule struct {
	name string
	path string
}

type Evaluator struct {
	ctx             *hcl.EvalContext
	blocks          block.Blocks
	modules         []*ModuleInfo
	visitedModules  []*visitedModule
	inputVars       map[string]cty.Value
	moduleMetadata  *ModulesMetadata
	projectRootPath string // root of the current scan
	stopOnHCLError  bool
}

func NewEvaluator(
	projectRootPath string,
	modulePath string,
	blocks block.Blocks,
	inputVars map[string]cty.Value,
	moduleMetadata *ModulesMetadata,
	modules []*ModuleInfo,
	visitedModules []*visitedModule,
	stopOnHCLError bool,
) *Evaluator {

	ctx := &hcl.EvalContext{
		Variables: make(map[string]cty.Value),
		Functions: Functions(modulePath),
	}

	for _, b := range blocks {
		b.AttachEvalContext(ctx)
	}

	return &Evaluator{
		projectRootPath: projectRootPath,
		ctx:             ctx,
		blocks:          blocks,
		inputVars:       inputVars,
		moduleMetadata:  moduleMetadata,
		modules:         modules,
		visitedModules:  visitedModules,
		stopOnHCLError:  stopOnHCLError,
	}
}

func (e *Evaluator) SetModuleBasePath(path string) {
	e.projectRootPath = path
}

func (e *Evaluator) evaluateStep(i int) {

	evalTime := metrics.Start(metrics.Evaluation)
	debug.Log("Starting iteration %d of hclcontext evaluation...", i+1)

	e.ctx.Variables["var"] = e.getValuesByBlockType("variable")
	e.ctx.Variables["local"] = e.getValuesByBlockType("locals")
	e.ctx.Variables["provider"] = e.getValuesByBlockType("provider")

	resources := e.getValuesByBlockType("resource")
	for key, resource := range resources.AsValueMap() {
		e.ctx.Variables[key] = resource
	}

	e.ctx.Variables["data"] = e.getValuesByBlockType("data")
	e.ctx.Variables["output"] = e.getValuesByBlockType("output")

	evalTime.Stop()

	e.evaluateModules()
}

func (e *Evaluator) evaluateModules() {

	for _, module := range e.modules {
		if visited := func(module *ModuleInfo) bool {
			for _, v := range e.visitedModules {
				if v.name == module.Name && v.path == module.Path {
					debug.Log("Module [%s:%s] has already been seen", v.name, v.path)
					return true
				}
			}
			return false
		}(module); visited {
			continue
		}

		e.visitedModules = append(e.visitedModules, &visitedModule{module.Name, module.Path})

		evalTime := metrics.Start(metrics.Evaluation)
		inputVars := make(map[string]cty.Value)
		for _, attr := range module.Definition.GetAttributes() {
			func() {
				defer func() {
					if err := recover(); err != nil {
						return
					}
				}()
				inputVars[attr.Name()] = attr.Value()
			}()
		}
		evalTime.Stop()

		childModules := LoadModules(module.Blocks, e.projectRootPath, e.moduleMetadata, e.stopOnHCLError)
		moduleEvaluator := NewEvaluator(e.projectRootPath, module.Path, module.Blocks, inputVars, e.moduleMetadata, childModules, e.visitedModules, e.stopOnHCLError)
		e.SetModuleBasePath(e.projectRootPath)
		b, _ := moduleEvaluator.EvaluateAll()
		e.blocks = mergeBlocks(e.blocks, b)

		evalTime = metrics.Start(metrics.Evaluation)
		// export module outputs
		moduleMapRaw := e.ctx.Variables["module"]
		if moduleMapRaw == cty.NilVal {
			moduleMapRaw = cty.ObjectVal(make(map[string]cty.Value))
		}
		moduleMap := moduleMapRaw.AsValueMap()
		if moduleMap == nil {
			moduleMap = make(map[string]cty.Value)
		}
		moduleMap[module.Name] = moduleEvaluator.ExportOutputs()
		e.ctx.Variables["module"] = cty.ObjectVal(moduleMap)
		evalTime.Stop()
	}
}

// export module outputs to a parent hclcontext
func (e *Evaluator) ExportOutputs() cty.Value {
	return e.ctx.Variables["output"]
}

func (e *Evaluator) EvaluateAll() (block.Blocks, error) {

	var lastContext hcl.EvalContext

	for i := 0; i < maxContextIterations; i++ {

		e.evaluateStep(i)

		// if ctx matches the last evaluation, we can bail, nothing left to resolve
		if reflect.DeepEqual(lastContext.Variables, e.ctx.Variables) {
			break
		}

		if len(e.ctx.Variables) != len(lastContext.Variables) {
			lastContext.Variables = make(map[string]cty.Value, len(e.ctx.Variables))
		}
		for k, v := range e.ctx.Variables {
			lastContext.Variables[k] = v
		}
	}

	var allBlocks block.Blocks
	allBlocks = e.blocks
	for _, module := range e.modules {
		allBlocks = mergeBlocks(allBlocks, module.Blocks)
	}

	return allBlocks, nil
}

func mergeBlocks(allBlocks block.Blocks, newBlocks block.Blocks) block.Blocks {
	var merger = make(map[*block.Block]bool)
	for _, b := range allBlocks {
		merger[b] = true
	}

	for _, b := range newBlocks {
		if _, ok := merger[b]; !ok {
			allBlocks = append(allBlocks, b)
		}
	}
	return allBlocks
}

// returns true if all evaluations were successful
func (e *Evaluator) getValuesByBlockType(blockType string) cty.Value {

	blocksOfType := e.blocks.OfType(blockType)
	values := make(map[string]cty.Value)

	for _, b := range blocksOfType {

		switch b.Type() {
		case "variable": // variables are special in that their value comes from the "default" attribute

			if b.Label() == "" {
				continue
			}

			attributes, _ := b.HCL().Body.JustAttributes()
			if attributes == nil {
				continue
			}

			if override, exists := e.inputVars[b.Label()]; exists {
				values[b.Label()] = override
			} else if def, exists := attributes["default"]; exists {
				values[b.Label()], _ = def.Expr.Value(e.ctx)
			}
		case "output":

			if b.Label() == "" {
				continue
			}

			attributes, _ := b.HCL().Body.JustAttributes()
			if attributes == nil {
				continue
			}

			if def, exists := attributes["value"]; exists {
				func() {
					defer func() {
						_ = recover()
					}()
					values[b.Label()], _ = def.Expr.Value(e.ctx)
				}()
			}

		case "locals":
			for key, val := range e.readValues(b.HCL()).AsValueMap() {
				values[key] = val
			}
		case "provider", "module":
			if b.Label() == "" {
				continue
			}
			values[b.Label()] = e.readValues(b.HCL())
		case "resource", "data":

			if len(b.HCL().Labels) < 2 {
				continue
			}

			blockMap, ok := values[b.HCL().Labels[0]]
			if !ok {
				values[b.HCL().Labels[0]] = cty.ObjectVal(make(map[string]cty.Value))
				blockMap = values[b.HCL().Labels[0]]
			}

			valueMap := blockMap.AsValueMap()
			if valueMap == nil {
				valueMap = make(map[string]cty.Value)
			}

			valueMap[b.HCL().Labels[1]] = e.readValues(b.HCL())
			values[b.HCL().Labels[0]] = cty.ObjectVal(valueMap)
		}

	}

	return cty.ObjectVal(values)

}

// returns true if all evaluations were successful
func (e *Evaluator) readValues(block *hcl.Block) cty.Value {

	values := make(map[string]cty.Value)

	attributes, diagnostics := block.Body.JustAttributes()
	if diagnostics != nil && diagnostics.HasErrors() {
		return cty.NilVal
	}

	for _, attribute := range attributes {
		func() {
			defer func() {
				if err := recover(); err != nil {
					return
				}
			}()
			val, _ := attribute.Expr.Value(e.ctx)
			values[attribute.Name] = val
		}()
	}

	return cty.ObjectVal(values)
}
