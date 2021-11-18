package iotago

import (
	"context"
	"errors"
	"fmt"

	"github.com/iotaledger/hive.go/serializer"
)

var (
	// ErrTransactionBuilderUnsupportedAddress gets returned when an unsupported address type
	// is given for a builder operation.
	ErrTransactionBuilderUnsupportedAddress = errors.New("unsupported address type")
	// ErrTransactionBuilder defines a generic error occurring within the TransactionBuilder.
	ErrTransactionBuilder = errors.New("transaction builder error")
)

// NewTransactionBuilder creates a new TransactionBuilder.
func NewTransactionBuilder() *TransactionBuilder {
	return &TransactionBuilder{
		essence: &TransactionEssence{
			Inputs:  Inputs{},
			Outputs: Outputs{},
			Payload: nil,
		},
		inputToAddr: map[OutputID]Address{},
	}
}

// TransactionBuilder is used to easily build up a Transaction.
type TransactionBuilder struct {
	occurredBuildErr error
	essence          *TransactionEssence
	inputToAddr      map[OutputID]Address
}

// ToBeSignedUTXOInput defines a UTXO input which needs to be signed.
type ToBeSignedUTXOInput struct {
	// The address to which this input belongs to.
	Address Address `json:"address"`
	// The actual UTXO input.
	Input *UTXOInput `json:"input"`
}

// AddInput adds the given input to the builder.
func (b *TransactionBuilder) AddInput(input *ToBeSignedUTXOInput) *TransactionBuilder {
	b.inputToAddr[input.Input.ID()] = input.Address
	b.essence.Inputs = append(b.essence.Inputs, input.Input)
	return b
}

// TransactionBuilderInputFilter is a filter function which determines whether
// an input should be used or not. (returning true = pass). The filter can also
// be used to accumulate data over the set of inputs, i.e. the input sum etc.
type TransactionBuilderInputFilter func(utxoInput *UTXOInput, input Output) bool

// AddInputsViaNodeQuery adds any unspent outputs by the given address as an input to the built transaction
// if it passes the filter function. It is the caller's job to ensure that the limit of returned outputs on the queried
// node is enough high for the application's purpose. filter can be nil.
func (b *TransactionBuilder) AddInputsViaNodeQuery(ctx context.Context, addr Address, nodeHTTPAPIClient *NodeHTTPAPIClient, filter TransactionBuilderInputFilter) *TransactionBuilder {
	switch x := addr.(type) {
	case *Ed25519Address:
	default:
		b.occurredBuildErr = fmt.Errorf("%w: auto. inputs via node query only supports Ed25519Address but got %T", ErrTransactionBuilderUnsupportedAddress, x)
	}

	_, unspentOutputs, err := nodeHTTPAPIClient.OutputsByEd25519Address(ctx, addr.(*Ed25519Address), false)
	if err != nil {
		b.occurredBuildErr = err
		return b
	}

	for utxoInput, output := range unspentOutputs {
		if filter != nil && !filter(utxoInput, output) {
			continue
		}

		b.AddInput(&ToBeSignedUTXOInput{Address: addr, Input: utxoInput})
	}

	return b
}

// AddOutput adds the given output to the builder.
func (b *TransactionBuilder) AddOutput(output Output) *TransactionBuilder {
	b.essence.Outputs = append(b.essence.Outputs, output)
	return b
}

// AddIndexationPayload adds the given Indexation as the inner payload.
func (b *TransactionBuilder) AddIndexationPayload(payload *Indexation) *TransactionBuilder {
	b.essence.Payload = payload
	return b
}

// TransactionFunc is a function which receives a Transaction as its parameter.
type TransactionFunc func(tx *Transaction)

// BuildAndSwapToMessageBuilder builds the transaction and then swaps to a MessageBuilder with
// the transaction set as its payload. txFunc can be nil.
func (b *TransactionBuilder) BuildAndSwapToMessageBuilder(deSeriParas *DeSerializationParameters, signer AddressSigner, txFunc TransactionFunc) *MessageBuilder {
	msgBuilder := NewMessageBuilder()
	tx, err := b.Build(deSeriParas, signer)
	if err != nil {
		msgBuilder.err = err
		return msgBuilder
	}
	if txFunc != nil {
		txFunc(tx)
	}
	return msgBuilder.Payload(tx)
}

// Build sings the inputs with the given signer and returns the built payload.
func (b *TransactionBuilder) Build(deSeriParas *DeSerializationParameters, signer AddressSigner) (*Transaction, error) {
	switch {
	case b.occurredBuildErr != nil:
		return nil, b.occurredBuildErr
	case deSeriParas == nil:
		return nil, fmt.Errorf("%w: must supply de/serialization parameters", ErrTransactionBuilder)
	case signer == nil:
		return nil, fmt.Errorf("%w: must supply signer", ErrTransactionBuilder)
	}

	// sort inputs and outputs by their serialized byte order
	txEssenceData, err := b.essence.SigningMessage()
	if err != nil {
		return nil, err
	}

	sigBlockPos := map[string]int{}
	unlockBlocks := UnlockBlocks{}
	for i, input := range b.essence.Inputs {
		addr := b.inputToAddr[input.(*UTXOInput).ID()]
		addrStr := addr.(fmt.Stringer).String()

		// check whether a previous signature unlock block
		// already signs inputs for the given address
		pos, alreadySigned := sigBlockPos[addrStr]
		if alreadySigned {
			// create a reference unlock block instead
			unlockBlocks = append(unlockBlocks, &ReferenceUnlockBlock{Reference: uint16(pos)})
			continue
		}

		// create a new signature for the given address
		var signature Signature
		signature, err = signer.Sign(addr, txEssenceData)
		if err != nil {
			return nil, err
		}

		unlockBlocks = append(unlockBlocks, &SignatureUnlockBlock{Signature: signature})
		sigBlockPos[addrStr] = i
	}

	sigTxPayload := &Transaction{Essence: b.essence, UnlockBlocks: unlockBlocks}

	if _, err := sigTxPayload.Serialize(serializer.DeSeriModePerformValidation, deSeriParas); err != nil {
		return nil, err
	}

	return sigTxPayload, nil
}
