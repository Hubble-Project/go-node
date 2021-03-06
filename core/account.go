package core

import (
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/BOPR/common"
	"github.com/BOPR/contracts/rollup"
	"github.com/jinzhu/gorm"
	gormbulk "github.com/t-tiger/gorm-bulk-insert"
)

// UserAccount is the user data stored on the node per user
type UserAccount struct {
	// ID is the path of the user account in the PDA Tree
	// Cannot be changed once created
	AccountID uint64 `gorm:"not null;index:AccountID"`

	Data []byte `gorm:"type:varbinary(255)" sql:"DEFAULT:0"`

	// Path from root to leaf
	// NOTE: not a part of the leaf
	// Path is a string to that we can run LIKE queries
	Path string `gorm:"not null;index:Path"`

	// Pending = 0 means has deposit but not merged to balance tree
	// Active = 1
	// InActive = 2 => non leaf node
	// NonInitialised = 100
	Status uint64 `gorm:"not null;index:Status"`

	// Type of nodes
	// 1 => terminal
	// 0 => root
	// 2 => non terminal
	Type uint64 `gorm:"not null;index:Type"`

	// keccak hash of the node
	Hash string `gorm:"not null;index:Hash"`

	Level uint64 `gorm:"not null;index:Level"`

	// Add the deposit hash for the account
	CreatedByDepositSubTree string
}

// NewUserAccount creates a new user account
func NewUserAccount(id, status uint64, path string, data []byte) *UserAccount {
	newAcccount := &UserAccount{
		AccountID: id,
		Path:      path,
		Status:    status,
		Type:      TYPE_TERMINAL,
		Data:      data,
	}
	newAcccount.UpdatePath(newAcccount.Path)
	newAcccount.CreateAccountHash()
	return newAcccount
}

// NewAccountNode creates a new non-terminal user account, the only this useful in this is
// Path, Status, Hash, PubkeyHash
func NewAccountNode(path, hash string) *UserAccount {
	newAcccount := &UserAccount{
		AccountID: ZERO,
		Path:      path,
		Status:    STATUS_ACTIVE,
		Type:      TYPE_NON_TERMINAL,
	}
	newAcccount.UpdatePath(newAcccount.Path)
	newAcccount.Hash = hash
	return newAcccount
}

// NewAccountNode creates a new terminal user account but in pending state
// It is to be used while adding new deposits while they are not finalised
func NewPendingUserAccount(id uint64, data []byte) *UserAccount {
	newAcccount := &UserAccount{
		AccountID: id,
		Path:      UNINITIALIZED_PATH,
		Status:    STATUS_PENDING,
		Type:      TYPE_TERMINAL,
		Data:      data,
	}
	newAcccount.UpdatePath(newAcccount.Path)
	newAcccount.CreateAccountHash()
	return newAcccount
}

func (acc *UserAccount) UpdatePath(path string) {
	acc.Path = path
	acc.Level = uint64(len(path))
}

func (acc *UserAccount) String() string {
	_, balance, nonce, token, _ := LoadedBazooka.DecodeAccount(acc.Data)
	return fmt.Sprintf("ID: %d Bal: %d Nonce: %d Token: %v Path: %v TokenType:%v NodeType: %v", acc.AccountID, balance, nonce, token, acc.Path, acc.Type, acc.Hash)
}

func (acc *UserAccount) ToABIAccount() (rollupTx rollup.TypesUserAccount, err error) {
	var ID, balance, nonce, token *big.Int = big.NewInt(0), big.NewInt(0), big.NewInt(0), big.NewInt(0)
	if acc.Type == TYPE_TERMINAL {
		ID, balance, nonce, token, err = LoadedBazooka.DecodeAccount(acc.Data)
		if err != nil {
			fmt.Println("unable to convert", err)
			return
		}
	}
	rollupTx.ID = ID
	rollupTx.Balance = balance
	rollupTx.Nonce = nonce
	rollupTx.TokenType = token
	return
}

func (acc *UserAccount) HashToByteArray() ByteArray {
	ba, err := HexToByteArray(acc.Hash)
	if err != nil {
		panic(err)
	}
	return ba
}

func (acc *UserAccount) IsCoordinator() bool {
	if acc.Path != "" {
		return false
	}

	if acc.Status != 1 {
		return false
	}

	if acc.Type != 0 {
		return false
	}

	return true
}

func (acc *UserAccount) AccountInclusionProof(path int64) (accInclusionProof rollup.TypesAccountInclusionProof, err error) {
	accABI, err := acc.ToABIAccount()
	if err != nil {
		return
	}
	accInclusionProof = rollup.TypesAccountInclusionProof{
		PathToAccount: big.NewInt(path),
		Account:       accABI,
	}
	return accInclusionProof, nil
}

func (acc *UserAccount) CreateAccountHash() {
	accountHash := common.Keccak256(acc.Data)
	acc.Hash = accountHash.String()
}

//
// Utils
//

// EmptyAcccount creates a new account which has the same hash as ZERO_VALUE_LEAF
func EmptyAccount() UserAccount {
	return *NewUserAccount(ZERO, STATUS_INACTIVE, "", []byte(""))
}

//
// DB interactions for account
//

// InitBalancesTree initialises the balances tree
func (db *DB) InitBalancesTree(depth uint64, genesisAccounts []UserAccount) error {
	// calculate total number of leaves
	totalLeaves := math.Exp2(float64(depth))
	if int(totalLeaves) != len(genesisAccounts) {
		return errors.New("Depth and number of leaves do not match")
	}
	db.Logger.Debug("Attempting to init balance tree", "totalAccounts", totalLeaves)

	var err error

	// insert coodinator leaf
	err = db.InsertCoordinatorAccounts(&genesisAccounts[0], depth)
	if err != nil {
		db.Logger.Error("Unable to insert coodinator account", "err", err)
		return err
	}

	var insertRecords []interface{}
	prevNodePath := genesisAccounts[0].Path

	for i := 1; i < len(genesisAccounts); i++ {
		pathToAdjacentNode, err := GetAdjacentNodePath(prevNodePath)
		if err != nil {
			return err
		}
		genesisAccounts[i].UpdatePath(pathToAdjacentNode)
		insertRecords = append(insertRecords, genesisAccounts[i])
		prevNodePath = genesisAccounts[i].Path
	}

	db.Logger.Info("Inserting all accounts to DB", "count", len(insertRecords))
	err = gormbulk.BulkInsert(db.Instance, insertRecords, len(insertRecords))
	if err != nil {
		db.Logger.Error("Unable to insert accounts to DB", "err", err)
		return errors.New("Unable to insert accounts")
	}

	// merkelise
	// 1. Pick all leaves at level depth
	// 2. Iterate 2 of them and create parents and store
	// 3. Persist all parents to database
	// 4. Start with next round
	for i := depth; i > 0; i-- {
		// get all leaves at depth N
		accs, err := db.GetAccountsAtDepth(i)
		if err != nil {
			return err
		}
		var nextLevelAccounts []interface{}

		// iterate over 2 at a time and create next level
		for i := 0; i < len(accs); i += 2 {
			left, err := HexToByteArray(accs[i].Hash)
			if err != nil {
				return err
			}
			right, err := HexToByteArray(accs[i+1].Hash)
			if err != nil {
				return err
			}
			parentHash, err := GetParent(left, right)
			if err != nil {
				return err
			}
			parentPath := GetParentPath(accs[i].Path)
			newAccNode := *NewAccountNode(parentPath, parentHash.String())
			nextLevelAccounts = append(nextLevelAccounts, newAccNode)
		}

		err = gormbulk.BulkInsert(db.Instance, nextLevelAccounts, len(nextLevelAccounts))
		if err != nil {
			db.Logger.Error("Unable to insert accounts to DB", "err", err)
			return errors.New("Unable to insert accounts")
		}
	}

	// mark the root node type correctly
	return nil
}

func (db *DB) GetAccountsAtDepth(depth uint64) ([]UserAccount, error) {
	var accs []UserAccount
	err := db.Instance.Where("level = ?", depth).Find(&accs).Error
	if err != nil {
		return accs, err
	}
	return accs, nil
}

func (db *DB) UpdateAccount(account UserAccount) error {
	db.Logger.Info("Updated account pubkey", "ID", account.AccountID)
	account.CreateAccountHash()
	siblings, err := db.GetSiblings(account.Path)
	if err != nil {
		return err
	}

	db.Logger.Debug("Updating account", "Hash", account.Hash, "Path", account.Path, "siblings", siblings, "countOfSiblings", len(siblings))
	return db.StoreLeaf(account, account.Path, siblings)
}

func (db *DB) StoreLeaf(account UserAccount, path string, siblings []UserAccount) error {
	var err error
	computedNode := account
	for i := 0; i < len(siblings); i++ {
		var parentHash ByteArray
		sibling := siblings[i]
		isComputedRightSibling := GetNthBitFromRight(
			path,
			i,
		)
		if isComputedRightSibling == 0 {
			parentHash, err = GetParent(computedNode.HashToByteArray(), sibling.HashToByteArray())
			if err != nil {
				return err
			}
			// Store the node!
			err = db.StoreNode(parentHash, computedNode, sibling)
			if err != nil {
				return err
			}
		} else {
			parentHash, err = GetParent(sibling.HashToByteArray(), computedNode.HashToByteArray())
			if err != nil {
				return err
			}
			// Store the node!
			err = db.StoreNode(parentHash, sibling, computedNode)
			if err != nil {
				return err
			}
		}

		parentAccount, err := db.GetAccountByPath(GetParentPath(computedNode.Path))
		if err != nil {
			return err
		}
		computedNode = parentAccount
	}

	// Store the new root
	err = db.UpdateRootNodeHashes(computedNode.HashToByteArray())
	if err != nil {
		return err
	}

	return nil
}

// StoreNode updates the nodes given the parent hash
func (db *DB) StoreNode(parentHash ByteArray, leftNode UserAccount, rightNode UserAccount) (err error) {
	// update left account
	err = db.updateAccount(leftNode, leftNode.Path)
	if err != nil {
		return err
	}
	// update right account
	err = db.updateAccount(rightNode, rightNode.Path)
	if err != nil {
		return err
	}
	// update the parent with the new hashes
	return db.UpdateParentWithHash(GetParentPath(leftNode.Path), parentHash)
}

func (db *DB) UpdateParentWithHash(pathToParent string, newHash ByteArray) error {
	// Update the root hash
	var tempAccount UserAccount
	tempAccount.Path = pathToParent
	tempAccount.Hash = newHash.String()
	return db.updateAccount(tempAccount, pathToParent)
}

func (db *DB) UpdateRootNodeHashes(newRoot ByteArray) error {
	var tempAccount UserAccount
	tempAccount.Path = ""
	tempAccount.Hash = newRoot.String()
	return db.updateAccount(tempAccount, tempAccount.Path)
}

func (db *DB) AddNewPendingAccount(acc UserAccount) error {
	return db.Instance.Create(&acc).Error
}

func (db *DB) GetSiblings(path string) ([]UserAccount, error) {
	var relativePath = path
	var siblings []UserAccount
	for i := len(path); i > 0; i-- {
		otherChild := GetOtherChild(relativePath)
		otherNode, err := db.GetAccountByPath(otherChild)
		if err != nil {
			return siblings, err
		}
		siblings = append(siblings, otherNode)
		relativePath = GetParentPath(relativePath)
	}
	return siblings, nil
}

// GetAccount gets the account of the given path from the DB
func (db *DB) GetAccountByPath(path string) (UserAccount, error) {
	var account UserAccount
	err := db.Instance.Where("path = ?", path).Find(&account).GetErrors()
	if len(err) != 0 {
		return account, ErrRecordNotFound(fmt.Sprintf("unable to find record for path: %v err:%v", path, err))
	}
	return account, nil
}

func (db *DB) GetAccountByHash(hash string) (UserAccount, error) {
	var account UserAccount
	if db.Instance.First(&account, hash).RecordNotFound() {
		return account, ErrRecordNotFound(fmt.Sprintf("unable to find record for hash: %v", hash))
	}
	return account, nil
}

func (db *DB) GetAccountByID(ID uint64) (UserAccount, error) {
	var account UserAccount
	if err := db.Instance.Where("account_id = ? AND status = ?", ID, STATUS_ACTIVE).Find(&account).Error; err != nil {
		return account, ErrRecordNotFound(fmt.Sprintf("unable to find record for ID: %v", ID))
	}
	return account, nil
}

func (db *DB) GetDepositSubTreeRoot(hash string, level uint64) (UserAccount, error) {
	var account UserAccount
	err := db.Instance.Where("level = ? AND hash = ?", level, hash).First(&account).Error
	if gorm.IsRecordNotFoundError(err) {
		return account, ErrRecordNotFound(fmt.Sprintf("unable to find record for hash: %v", hash))
	}
	return account, nil
}

func (db *DB) GetRoot() (UserAccount, error) {
	var account UserAccount
	err := db.Instance.Where("level = ?", 0).Find(&account).GetErrors()
	if len(err) != 0 {
		return account, ErrRecordNotFound(fmt.Sprintf("unable to find record. err:%v", err))
	}
	return account, nil
}

func (db *DB) InsertCoordinatorAccounts(acc *UserAccount, depth uint64) error {
	acc.UpdatePath(GenCoordinatorPath(depth))
	acc.CreateAccountHash()
	acc.Type = TYPE_TERMINAL
	return db.Instance.Create(&acc).Error
}

// updateAccount will simply replace all the changed fields
func (db *DB) updateAccount(newAcc UserAccount, path string) error {
	return db.Instance.Model(&newAcc).Where("path = ?", path).Update(newAcc).Error
}

func (db *DB) GetAccountCount() (int, error) {
	var count int
	db.Instance.Table("user_accounts").Count(&count)
	return count, nil
}

func (db *DB) DeletePendingAccount(ID uint64) error {
	var account UserAccount
	if err := db.Instance.Where("account_id = ? AND status = ?", ID, STATUS_PENDING).Delete(&account).Error; err != nil {
		return ErrRecordNotFound(fmt.Sprintf("unable to delete record for ID: %v", ID))
	}
	return nil
}

//
// Deposit Account Handling
//

func (db *DB) AttachDepositInfo(root ByteArray) error {
	// find all pending accounts
	var account UserAccount
	account.CreatedByDepositSubTree = root.String()
	if err := db.Instance.Model(&account).Where("status = ?", STATUS_PENDING).Update(&account).Error; err != nil {
		return err
	}
	return nil
}

func (db *DB) GetPendingAccByDepositRoot(root ByteArray) ([]UserAccount, error) {
	// find all accounts with CreatedByDepositSubTree as `root`
	var pendingAccounts []UserAccount

	if err := db.Instance.Where("created_by_deposit_sub_tree = ? AND status = ?", root.String(), STATUS_PENDING).Find(&pendingAccounts).Error; err != nil {
		return pendingAccounts, err
	}

	return pendingAccounts, nil
}
