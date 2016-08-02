// Copyright (c) 2013-2015 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package txscript

import (
	"fmt"

	"chain/cos/bc"
)

// ScriptFlags is a bitmask defining additional operations or tests that will be
// done when executing a script pair.
type ScriptFlags uint32

const (
	// ScriptStrictMultiSig defines whether to verify the stack item
	// used by CHECKMULTISIG is zero length.
	ScriptStrictMultiSig ScriptFlags = 1 << iota

	// ScriptDiscourageUpgradableNops defines whether to verify that
	// NOP1 through NOP10 are reserved for future soft-fork upgrades.  This
	// flag must not be used for consensus critical code nor applied to
	// blocks as this flag is only for stricter standard transaction
	// checks.  This flag is only applied when the above opcodes are
	// executed.
	ScriptDiscourageUpgradableNops

	// ScriptVerifyDERSignatures defines that signatures are required
	// to compily with the DER format.
	ScriptVerifyDERSignatures

	// ScriptVerifyLowS defines that signtures are required to comply with
	// the DER format and whose S value is <= order / 2.  This is rule 5
	// of BIP0062.
	ScriptVerifyLowS

	// ScriptVerifySigPushOnly defines that signature scripts must contain
	// only pushed data.  This is rule 2 of BIP0062.
	ScriptVerifySigPushOnly

	// ScriptVerifyStrictEncoding defines that signature scripts and
	// public keys must follow the strict encoding requirements.
	ScriptVerifyStrictEncoding
)

const (
	// maxExecutionStackSize is the maximum number of stack frames that
	// can be on the execution stack.
	maxExecutionStackSize = 10
)

type (
	// Engine is the virtual machine that executes scripts.
	Engine struct {
		scriptVersion    []byte
		scriptVersionVal scriptNum   // optimization - the scriptNum value of scriptVersion
		estack           scriptStack // execution stack
		dstack           stack       // data stack
		astack           stack       // alternate data stack
		tx               *bc.TxData
		block            *bc.Block
		sigHasher        *bc.SigHasher
		txIdx            int
		numOps           int
		flags            ScriptFlags
		available        []uint64 // mutable copy of each output's Amount field, used for OP_RESERVEOUTPUT reservations
	}
)

func isKnownVersion(version int64) bool {
	return version >= 0 && version <= 2
}

func (vm *Engine) currentVersion() scriptNum {
	return vm.scriptVersionVal
}

func (vm *Engine) currentTxInput() *bc.TxInput {
	return vm.tx.Inputs[vm.txIdx]
}

// hasFlag returns whether the script engine instance has the passed flag set.
func (vm *Engine) hasFlag(flag ScriptFlags) bool {
	return vm.flags&flag == flag
}

// isBranchExecuting returns whether or not the current conditional branch is
// actively executing.  For example, when the data stack has an OP_FALSE on it
// and an OP_IF is encountered, the branch is inactive until an OP_ELSE or
// OP_ENDIF is encountered.  It properly handles nested conditionals.
func (vm *Engine) isBranchExecuting() bool {
	s := vm.estack.Peek().condStack
	if len(s) == 0 {
		return true
	}
	v := s[len(s)-1]
	return v == OpCondIfTrue || v == OpCondWhileTrue
}

// executeOpcode peforms execution on the passed opcode.  It takes into account
// whether or not it is hidden by conditionals, but some rules still must be
// tested in this case.
func (vm *Engine) executeOpcode(pop *parsedOpcode) error {
	// Disabled opcodes are fail on program counter.
	if pop.isDisabled(int(vm.currentVersion()), vm.block != nil) {
		return ErrStackOpDisabled
	}

	// Always-illegal opcodes are fail on program counter.
	if pop.alwaysIllegal() {
		return ErrStackReservedOpcode
	}

	// Note that this includes OP_RESERVED which counts as a push operation.
	if pop.opcode.value > OP_16 {
		vm.numOps++
		if vm.numOps > maxOpsPerScript {
			return ErrStackTooManyOperations
		}

	} else if len(pop.data) > MaxScriptElementSize {
		return ErrStackElementTooBig
	}

	// Nothing left to do when this is not a conditional opcode and it is
	// not in an executing branch.
	if !vm.isBranchExecuting() && !pop.isConditional() {
		return nil
	}

	return pop.opcode.opfunc(pop, vm)
}

// DisasmPC returns the string for the disassembly of the opcode that will be
// next to execute when Step() is called.
func (vm *Engine) DisasmPC() (string, error) {
	frame, off, err := vm.estack.curPC()
	if err != nil {
		return "", err
	}
	return vm.estack.disasm(frame, off), nil
}

// DisasmScript returns the disassembly string for the entire script.
func (vm *Engine) DisasmScript() (string, error) {
	var disstr string
	for fIdx := range vm.estack.frames {
		frame := vm.estack.frames[len(vm.estack.frames)-fIdx-1]
		for idx := range frame.script {
			disstr = disstr + frame.disasm(idx) + "\n"
		}
	}
	return disstr, nil
}

// CheckErrorCondition returns nil if the running script has ended and was
// successful, leaving a true boolean on the stack.  An error otherwise,
// including if the script has not finished.
func (vm *Engine) CheckErrorCondition(finalScript bool) error {
	// Check execution is actually done.
	if !vm.estack.empty() {
		return ErrStackScriptUnfinished
	}
	if vm.dstack.Depth() < 1 {
		return ErrStackEmptyStack
	}

	v, err := vm.dstack.PopBool()
	if err != nil {
		return err
	}
	if !v {
		// Log interesting data.
		log.Tracef("%v", newLogClosure(func() string {
			dis, _ := vm.DisasmScript()
			return fmt.Sprintf("scripts failed: %s\n", dis)
		}))
		return ErrStackScriptFailed
	}
	return nil
}

// PushScript is called by OP_CHECKPREDICATE. It adds a new stack
// frame to the top of the execution stack.
func (vm *Engine) PushScript(newScript []parsedOpcode) {
	vm.estack.Push(&stackFrame{script: newScript})
}

// Step will execute the next instruction and move the program counter to the
// next opcode in the script, or the next script if the current has ended.  Step
// will return true in the case that the last opcode was successfully executed.
//
// The result of calling Step or any other method is undefined if an error is
// returned.
func (vm *Engine) Step() (done bool, err error) {

	// Verify that it is pointing to a valid address.
	_, off, err := vm.estack.curPC()
	if err != nil {
		return true, err
	}
	frame := vm.estack.Peek()
	opcode := frame.opcode(off)

	// Execute the opcode while taking into account several things such as
	// disabled opcodes, illegal opcodes, maximum allowed operations per
	// script, maximum script element sizes, and conditionals.
	err = vm.executeOpcode(opcode)
	if err != nil {
		return true, err
	}

	// The number of elements in the combination of the data and alt stacks
	// must not exceed the maximum number of stack elements allowed.
	if int(vm.dstack.Depth()+vm.astack.Depth()) > maxStackSize {
		return false, ErrStackOverflow
	}
	// The number of stack frames is also limited.
	if vm.estack.Depth() > maxExecutionStackSize {
		return false, ErrStackOverflow
	}

	// Move on to the next instruction.
	frame.step()

	// If we're finished with the frame, pop off stack frames until we find
	// one that is not finished yet.
	if frame.done() {
		return vm.estack.nextFrame()
	}
	return false, nil
}

// Execute will execute all scripts in the script engine and return either nil
// for successful validation or an error if one occurred.
func (vm *Engine) Execute() (err error) {
	// treat unknown versions as anyone can spend
	if !isKnownVersion(int64(vm.scriptVersionVal)) {
		return nil
	}

	done := false

	for !done {
		log.Tracef("%v", newLogClosure(func() string {
			dis, err := vm.DisasmPC()
			if err != nil {
				return fmt.Sprintf("stepping (%v)", err)
			}
			var execflag string
			if !vm.isBranchExecuting() {
				execflag = "!"
			}
			return fmt.Sprintf("%sstepping %v", execflag, dis)
		}))

		done, err = vm.Step()
		if err != nil {
			return err
		}
		log.Tracef("%v", newLogClosure(func() string {
			var dstr, astr string

			// if we're tracing, dump the stacks.
			if vm.dstack.Depth() != 0 {
				dstr = "Stack:\n" + vm.dstack.String()
			}
			if vm.astack.Depth() != 0 {
				astr = "AltStack:\n" + vm.astack.String()
			}

			return dstr + astr
		}))
	}

	return vm.CheckErrorCondition(true)
}

// checkHashTypeEncoding returns whether or not the passed hashtype adheres to
// the strict encoding requirements if enabled.
func (vm *Engine) checkHashTypeEncoding(hashType bc.SigHashType) error {
	if !vm.hasFlag(ScriptVerifyStrictEncoding) {
		return nil
	}

	sigHashType := hashType & ^bc.SigHashAnyOneCanPay
	if sigHashType < bc.SigHashAll || sigHashType > bc.SigHashSingle {
		return fmt.Errorf("invalid hashtype: 0x%x\n", hashType)
	}
	return nil
}

// getStack returns the contents of stack as a byte array bottom up
func getStack(stack *stack) [][]byte {
	array := make([][]byte, stack.Depth())
	for i := range array {
		// PeekByteArry can't fail due to overflow, already checked
		array[len(array)-i-1], _ = stack.PeekByteArray(int32(i))
	}
	return array
}

// setStack sets the stack to the contents of the array where the last item in
// the array is the top item in the stack.
func setStack(stack *stack, data [][]byte) {
	// This can not error. Only errors are for invalid arguments.
	_ = stack.DropN(stack.Depth())

	for i := range data {
		stack.PushByteArray(data[i])
	}
}

// GetStack returns the contents of the primary stack as an array. where the
// last item in the array is the top of the stack.
func (vm *Engine) GetStack() [][]byte {
	return getStack(&vm.dstack)
}

// SetStack sets the contents of the primary stack to the contents of the
// provided array where the last item in the array will be the top of the stack.
func (vm *Engine) SetStack(data [][]byte) {
	setStack(&vm.dstack, data)
}

// GetAltStack returns the contents of the alt stack as an array. where the
// last item in the array is the top of the stack.
func (vm *Engine) GetAltStack() [][]byte {
	return getStack(&vm.astack)
}

// SetAltStack sets the contents of the alt stack to the contents of the
// provided array where the last item in the array will be the top of the stack.
func (vm *Engine) SetAltStack(data [][]byte) {
	setStack(&vm.astack, data)
}

// Prepare prepares a previously allocated Engine for reuse with
// another txin, preserving state (to wit, vm.available) that P2C
// wants to save between txins.
func (vm *Engine) Prepare(script []byte, args [][]byte, txIdx int) error {
	// The provided transaction input index must refer to a valid input.
	if txIdx < 0 || (vm.tx != nil && txIdx >= len(vm.tx.Inputs)) {
		return ErrInvalidIndex
	}
	vm.txIdx = txIdx

	vm.dstack.Reset()
	vm.estack.Reset()

	for _, arg := range args {
		vm.dstack.PushByteArray(arg)
	}

	if len(script) > bc.MaxProgramByteLength {
		return ErrStackLongScript
	}
	parsedScript, err := parseScript(script)
	if err != nil {
		return err
	}

	vm.scriptVersion = parseScriptVersion(parsedScript)
	vm.scriptVersionVal, _ = makeScriptNum(vm.scriptVersion, false) // swallow errors

	vm.PushScript(parsedScript)

	vm.numOps = 0

	return nil
}

// NewReusableEngine allocates an Engine object that can execute scripts
// for every input  of a transaction.  Illustration (with error-checking
// elided for clarity):
//   engine, err := NewReusableEngine(tx, flags)
//   for i, txin := range tx.Inputs {
//     err = engine.Prepare(scriptPubKey, args, i)
//     err = engine.Execute()
//   }
// Note: every call to Execute() must be preceded by a call to
// Prepare() (including the first one).
func NewReusableEngine(tx *bc.TxData, flags ScriptFlags) (*Engine, error) {
	return newReusableEngine(tx, nil, flags)
}

func newReusableEngine(tx *bc.TxData, block *bc.Block, flags ScriptFlags) (*Engine, error) {
	vm := &Engine{
		tx:    tx,
		block: block,
		flags: flags,
	}
	if tx != nil {
		vm.sigHasher = bc.NewSigHasher(tx)
	}

	if vm.tx != nil {
		vm.available = make([]uint64, len(tx.Outputs))
		for i, output := range tx.Outputs {
			vm.available[i] = output.Amount
		}
	}

	return vm, nil
}

// NewEngine returns a new script engine for the provided public key script,
// transaction, and input index.  The flags modify the behavior of the script
// engine according to the description provided by each flag.
//
// This is equivalent to calling NewReusableEngine() followed by a
// call to Prepare().
func NewEngine(scriptPubKey []byte, tx *bc.TxData, txIdx int, flags ScriptFlags) (*Engine, error) {
	vm, err := NewReusableEngine(tx, flags)
	if err != nil {
		return nil, err
	}

	err = vm.Prepare(scriptPubKey, tx.Inputs[txIdx].InputWitness, txIdx)
	return vm, err
}

// NewEngineForBlock returns a new script engine for the provided block
// and its script. The flags modify the behavior of the script engine
// according to the description provided by each flag.
func NewEngineForBlock(scriptPubKey []byte, block *bc.Block, flags ScriptFlags) (*Engine, error) {
	vm, err := newReusableEngine(nil, block, flags)
	if err != nil {
		return nil, err
	}
	pushed, err := PushedData(block.SignatureScript)
	if err != nil {
		return nil, err
	}
	err = vm.Prepare(scriptPubKey, pushed, 0)
	return vm, err
}
