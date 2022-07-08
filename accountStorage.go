package emulator

import (
	"fmt"
	"github.com/onflow/cadence/runtime"
	"github.com/onflow/cadence/runtime/common"
	"github.com/onflow/cadence/runtime/interpreter"
	sdk "github.com/onflow/flow-go-sdk"
	"github.com/onflow/flow-go/model/flow"
)

type StorageItem map[string]string

func (s StorageItem) Get(key string) string {
	return s[key]
}

// NewAccountStorage creates an instance of the storage that holds the values for each storage path.
func NewAccountStorage(
	address sdk.Address,
	account *flow.Account,
	storage *runtime.Storage,
	inter *interpreter.Interpreter,
) (*AccountStorage, error) {
	addr, err := common.BytesToAddress(address.Bytes())
	if err != nil {
		return nil, err
	}

	extractStorage := func(path common.PathDomain) StorageItem {
		storageMap := storage.GetStorageMap(addr, path.Identifier(), false)
		if storageMap == nil {
			return nil
		}

		iterator := storageMap.Iterator(nil)
		values := make(StorageItem)
		k, v := iterator.Next()
		for v != nil {
			exportedValue, err := runtime.ExportValue(v, inter)
			if err != nil {
				continue // just skip errored value
			}

			values[k] = fmt.Sprintf("%s", exportedValue) //.(Stringer)
			//String() (Stringer)

			k, v = iterator.Next()
		}
		return values
	}

	return &AccountStorage{
		//Account: account,
		//Address: address,
		Private: extractStorage(common.PathDomainPrivate),
		Public:  extractStorage(common.PathDomainPublic),
		Storage: extractStorage(common.PathDomainStorage),
	}, nil
}

type AccountStorage struct {
	//	Address sdk.Address
	Private StorageItem
	Public  StorageItem
	Storage StorageItem
	//	Account *flow.Account
}
