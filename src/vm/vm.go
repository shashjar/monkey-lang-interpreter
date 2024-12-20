package vm

import (
	"fmt"
	"monkey/bytecode"
	"monkey/compiler"
	"monkey/object"
)

const StackSize = 2048
const GlobalsSize = 65536
const MaxFrames = 1024

var True = &object.Boolean{Value: true}
var False = &object.Boolean{Value: false}

var Null = &object.Null{}

// Represents a virtual machine used to execute bytecode instructions generated by the Monkey programming language compiler.
type VM struct {
	constants []object.Object

	stack []object.Object
	sp    int // Always points to the next free slot in the stack. The top of the stack is stack[sp - 1].

	globals []object.Object

	frames      []*Frame
	framesIndex int
}

func NewVM(bytecode *compiler.Bytecode) *VM {
	mainFn := &object.CompiledFunction{Instructions: bytecode.Instructions}
	mainClosure := &object.Closure{Fn: mainFn}
	mainFrame := NewFrame(mainClosure, 0)

	frames := make([]*Frame, MaxFrames)
	frames[0] = mainFrame

	return &VM{
		constants: bytecode.Constants,

		stack: make([]object.Object, StackSize),
		sp:    0,

		globals: make([]object.Object, GlobalsSize),

		frames:      frames,
		framesIndex: 1,
	}
}

func NewVMWithGlobalsStore(bytecode *compiler.Bytecode, globals []object.Object) *VM {
	vm := NewVM(bytecode)
	vm.globals = globals
	return vm
}

func (vm *VM) Run() error {
	var ip int
	var instr bytecode.Instructions
	var op bytecode.Opcode

	for vm.currentFrame().ip < len(vm.currentFrame().Instructions())-1 {
		vm.currentFrame().ip += 1

		ip = vm.currentFrame().ip
		instr = vm.currentFrame().Instructions()
		op = bytecode.Opcode(instr[ip])

		switch op {
		case bytecode.OpConstant:
			constIndex := bytecode.ReadUint16(instr[ip+1:])
			vm.currentFrame().ip += 2

			err := vm.push(vm.constants[constIndex])
			if err != nil {
				return err
			}
		case bytecode.OpTrue:
			err := vm.push(True)
			if err != nil {
				return err
			}
		case bytecode.OpFalse:
			err := vm.push(False)
			if err != nil {
				return err
			}
		case bytecode.OpNull:
			err := vm.push(Null)
			if err != nil {
				return err
			}

		case bytecode.OpPop:
			vm.pop()
		case bytecode.OpJumpNotTruthy:
			jumpToPos := int(bytecode.ReadUint16(instr[ip+1:]))
			vm.currentFrame().ip += 2

			condition := vm.pop()
			if !isTruthy(condition) {
				vm.currentFrame().ip = jumpToPos - 1 // Set to `pos - 1` since this loop increments ip on each iteration
			}
		case bytecode.OpJump:
			jumpToPos := int(bytecode.ReadUint16(instr[ip+1:]))
			vm.currentFrame().ip = jumpToPos - 1 // Set to `pos - 1` since this loop increments ip on each iteration

		case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv:
			err := vm.executeBinaryOperation(op)
			if err != nil {
				return err
			}
		case bytecode.OpEqual, bytecode.OpNotEqual, bytecode.OpGreaterThan:
			err := vm.executeComparison(op)
			if err != nil {
				return err
			}

		case bytecode.OpMinus:
			err := vm.executeMinusOperator()
			if err != nil {
				return err
			}
		case bytecode.OpBang:
			err := vm.executeBangOperator()
			if err != nil {
				return err
			}

		case bytecode.OpGetGlobal:
			globalIndex := int(bytecode.ReadUint16(instr[ip+1:]))
			vm.currentFrame().ip += 2

			err := vm.push(vm.globals[globalIndex])
			if err != nil {
				return err
			}
		case bytecode.OpSetGlobal:
			globalIndex := int(bytecode.ReadUint16(instr[ip+1:]))
			vm.currentFrame().ip += 2

			vm.globals[globalIndex] = vm.pop()
		case bytecode.OpGetLocal:
			localIndex := int(bytecode.ReadUint8(instr[ip+1:]))
			vm.currentFrame().ip += 1

			frame := vm.currentFrame()
			err := vm.push(vm.stack[frame.basePointer+localIndex])
			if err != nil {
				return err
			}
		case bytecode.OpSetLocal:
			localIndex := int(bytecode.ReadUint8(instr[ip+1:]))
			vm.currentFrame().ip += 1

			frame := vm.currentFrame()
			vm.stack[frame.basePointer+localIndex] = vm.pop()

		case bytecode.OpArray:
			numElements := int(bytecode.ReadUint16(instr[ip+1:]))
			vm.currentFrame().ip += 2

			array := vm.buildArray(vm.sp-numElements, vm.sp)
			vm.sp = vm.sp - numElements

			err := vm.push(array)
			if err != nil {
				return err
			}
		case bytecode.OpHashMap:
			numElements := int(bytecode.ReadUint16(instr[ip+1:]))
			vm.currentFrame().ip += 2

			hashmap, err := vm.buildHashMap(vm.sp-numElements, vm.sp)
			if err != nil {
				return err
			}
			vm.sp -= numElements

			err = vm.push(hashmap)
			if err != nil {
				return err
			}
		case bytecode.OpIndex:
			index := vm.pop()
			left := vm.pop()

			err := vm.executeIndexExpression(left, index)
			if err != nil {
				return err
			}

		case bytecode.OpCall:
			numArgs := int(bytecode.ReadUint8(instr[ip+1:]))
			vm.currentFrame().ip += 1

			err := vm.executeCall(numArgs)
			if err != nil {
				return err
			}
		case bytecode.OpReturnValue:
			returnValue := vm.pop()

			frame := vm.popFrame()        // Pop the function frame that has just finished execution
			vm.sp = frame.basePointer - 1 // Reset the stack pointer to where it was prior to entering this function (-1 to pop off the function itself as well)

			err := vm.push(returnValue) // Put the function return value at the top of the stack
			if err != nil {
				return err
			}
		case bytecode.OpReturn:
			frame := vm.popFrame()
			vm.sp = frame.basePointer - 1

			err := vm.push(Null)
			if err != nil {
				return err
			}
		case bytecode.OpGetBuiltIn:
			builtInIndex := int(bytecode.ReadUint8(instr[ip+1:]))
			vm.currentFrame().ip += 1

			definition := object.BuiltIns[builtInIndex]

			err := vm.push(definition.BuiltIn)
			if err != nil {
				return err
			}
		case bytecode.OpClosure:
			constIndex := int(bytecode.ReadUint16(instr[ip+1:]))
			// numFreeVars := bytecode.ReadUint8(instr[ip+3:])
			vm.currentFrame().ip += 3

			err := vm.pushClosure(constIndex)
			if err != nil {
				return err
			}

		default:
			return fmt.Errorf("invalid opcode received: %d", op)
		}
	}

	return nil
}

func (vm *VM) StackTop() object.Object {
	if vm.sp == 0 {
		return nil
	}
	return vm.stack[vm.sp-1]
}

func (vm *VM) LastPoppedStackElem() object.Object {
	return vm.stack[vm.sp]
}

func (vm *VM) push(obj object.Object) error {
	if vm.sp >= StackSize {
		return fmt.Errorf("stack overflow")
	}

	vm.stack[vm.sp] = obj
	vm.sp += 1

	return nil
}

func (vm *VM) pushClosure(constIndex int) error {
	constant := vm.constants[constIndex]
	function, ok := constant.(*object.CompiledFunction)
	if !ok {
		return fmt.Errorf("not a function: %+v", function)
	}

	closure := &object.Closure{Fn: function}
	return vm.push(closure)
}

func (vm *VM) pop() object.Object {
	obj := vm.stack[vm.sp-1]
	vm.sp -= 1
	return obj
}

func (vm *VM) currentFrame() *Frame {
	return vm.frames[vm.framesIndex-1]
}

func (vm *VM) pushFrame(f *Frame) {
	vm.frames[vm.framesIndex] = f
	vm.framesIndex += 1
}

func (vm *VM) popFrame() *Frame {
	vm.framesIndex -= 1
	return vm.frames[vm.framesIndex]
}

func nativeBoolToBooleanObject(input bool) *object.Boolean {
	if input {
		return True
	} else {
		return False
	}
}

func isTruthy(obj object.Object) bool {
	switch obj := obj.(type) {
	case *object.Boolean:
		return obj.Value
	case *object.Null:
		return false
	default:
		return true
	}
}

func (vm *VM) executeBinaryOperation(op bytecode.Opcode) error {
	right := vm.pop()
	left := vm.pop()

	leftType := left.Type()
	rightType := right.Type()

	switch {
	case leftType == object.INTEGER_OBJ && rightType == object.INTEGER_OBJ:
		return vm.executeBinaryIntegerOperation(op, left, right)
	case leftType == object.STRING_OBJ && rightType == object.STRING_OBJ:
		return vm.executeBinaryStringOperation(op, left, right)
	default:
		return fmt.Errorf("unsupported types for binary operation: %s %s", leftType, rightType)
	}
}

func (vm *VM) executeBinaryIntegerOperation(op bytecode.Opcode, left object.Object, right object.Object) error {
	leftValue := left.(*object.Integer).Value
	rightValue := right.(*object.Integer).Value

	var result int64

	switch op {
	case bytecode.OpAdd:
		result = leftValue + rightValue
	case bytecode.OpSub:
		result = leftValue - rightValue
	case bytecode.OpMul:
		result = leftValue * rightValue
	case bytecode.OpDiv:
		result = leftValue / rightValue
	default:
		return fmt.Errorf("unknown binary integer operator: %d", op)
	}

	return vm.push(&object.Integer{Value: result})
}

func (vm *VM) executeBinaryStringOperation(op bytecode.Opcode, left object.Object, right object.Object) error {
	leftValue := left.(*object.String).Value
	rightValue := right.(*object.String).Value

	switch op {
	case bytecode.OpAdd:
		return vm.push(&object.String{Value: leftValue + rightValue})
	default:
		return fmt.Errorf("unknown binary string operator: %d", op)
	}
}

func (vm *VM) executeComparison(op bytecode.Opcode) error {
	right := vm.pop()
	left := vm.pop()

	leftType := left.Type()
	rightType := right.Type()

	if leftType == object.INTEGER_OBJ && rightType == object.INTEGER_OBJ {
		return vm.executeIntegerComparison(op, left, right)
	} else if leftType == object.BOOLEAN_OBJ && rightType == object.BOOLEAN_OBJ {
		return vm.executeBooleanComparison(op, left, right)
	}

	return fmt.Errorf("unsupported types for binary comparison: %s %s", leftType, rightType)
}

func (vm *VM) executeIntegerComparison(op bytecode.Opcode, left object.Object, right object.Object) error {
	leftValue := left.(*object.Integer).Value
	rightValue := right.(*object.Integer).Value

	switch op {
	case bytecode.OpEqual:
		return vm.push(nativeBoolToBooleanObject(leftValue == rightValue))
	case bytecode.OpNotEqual:
		return vm.push(nativeBoolToBooleanObject(leftValue != rightValue))
	case bytecode.OpGreaterThan:
		return vm.push(nativeBoolToBooleanObject(leftValue > rightValue))
	default:
		return fmt.Errorf("unknown binary integer comparison operator: %d", op)
	}
}

func (vm *VM) executeBooleanComparison(op bytecode.Opcode, left object.Object, right object.Object) error {
	leftValue := left.(*object.Boolean).Value
	rightValue := right.(*object.Boolean).Value
	switch op {
	case bytecode.OpEqual:
		return vm.push(nativeBoolToBooleanObject(leftValue == rightValue))
	case bytecode.OpNotEqual:
		return vm.push(nativeBoolToBooleanObject(leftValue != rightValue))
	default:
		return fmt.Errorf("unknown binary boolean comparison operator: %d", op)
	}
}

func (vm *VM) executeMinusOperator() error {
	operand := vm.pop()

	if operand.Type() != object.INTEGER_OBJ {
		return fmt.Errorf("unsupported type for negation: %s", operand.Type())
	}

	value := operand.(*object.Integer).Value
	return vm.push(&object.Integer{Value: -value})
}

func (vm *VM) executeBangOperator() error {
	operand := vm.pop()

	switch operand {
	case True:
		return vm.push(False)
	case False:
		return vm.push(True)
	case Null:
		return vm.push(True)
	default:
		return vm.push(False)
	}
}

func (vm *VM) buildArray(startIndex int, endIndex int) object.Object {
	elements := make([]object.Object, endIndex-startIndex)

	for i := startIndex; i < endIndex; i++ {
		elements[i-startIndex] = vm.stack[i]
	}

	return &object.Array{Elements: elements}
}

func (vm *VM) buildHashMap(startIndex int, endIndex int) (object.Object, error) {
	kvPairs := make(map[object.HashKey]object.HashMapPair)

	for i := startIndex; i < endIndex; i += 2 {
		key := vm.stack[i]
		value := vm.stack[i+1]

		hashKey, ok := key.(object.Hashable)
		if !ok {
			return nil, fmt.Errorf("unusable as hash key: %s", key.Type())
		}

		pair := object.HashMapPair{Key: key, Value: value}
		kvPairs[hashKey.HashKey()] = pair
	}

	return &object.HashMap{KVPairs: kvPairs}, nil
}

func (vm *VM) executeIndexExpression(left object.Object, index object.Object) error {
	switch {
	case left.Type() == object.ARRAY_OBJ && index.Type() == object.INTEGER_OBJ:
		return vm.executeArrayIndex(left, index)
	case left.Type() == object.HASHMAP_OBJ:
		return vm.executeHashMapIndex(left, index)
	default:
		return fmt.Errorf("index operator not supported: %s", left.Type())
	}
}

func (vm *VM) executeArrayIndex(array object.Object, index object.Object) error {
	arrayObject := array.(*object.Array)
	i := index.(*object.Integer).Value
	max := int64(len(arrayObject.Elements) - 1)

	if i < 0 || i > max {
		return vm.push(Null)
	}

	return vm.push(arrayObject.Elements[i])
}

func (vm *VM) executeHashMapIndex(hashmap object.Object, index object.Object) error {
	hashmapObject := hashmap.(*object.HashMap)

	key, ok := index.(object.Hashable)
	if !ok {
		return fmt.Errorf("unusable as hash key: %s", index.Type())
	}

	pair, ok := hashmapObject.KVPairs[key.HashKey()]
	if !ok {
		return vm.push(Null)
	}

	return vm.push(pair.Value)
}

func (vm *VM) executeCall(numArgs int) error {
	callee := vm.stack[vm.sp-1-numArgs]
	switch callee := callee.(type) {
	case *object.Closure:
		return vm.callClosure(callee, numArgs)
	case *object.BuiltIn:
		return vm.callBuiltIn(callee, numArgs)
	default:
		return fmt.Errorf("attempted to call non-closure and non-builtin")
	}
}

func (vm *VM) callClosure(cl *object.Closure, numArgs int) error {
	if numArgs != cl.Fn.NumParameters {
		return fmt.Errorf("wrong number of arguments: expected=%d, got=%d", cl.Fn.NumParameters, numArgs)
	}

	frame := NewFrame(cl, vm.sp-numArgs)
	vm.pushFrame(frame)
	vm.sp = frame.basePointer + cl.Fn.NumLocals

	return nil
}

func (vm *VM) callBuiltIn(builtin *object.BuiltIn, numArgs int) error {
	args := vm.stack[vm.sp-numArgs : vm.sp]

	result := builtin.Fn(args...)
	vm.sp = vm.sp - numArgs - 1

	if result != nil {
		vm.push(result)
	} else {
		vm.push(Null)
	}

	return nil
}
