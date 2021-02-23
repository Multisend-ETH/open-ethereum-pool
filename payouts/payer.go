package payouts

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"os"
	"strconv"
	"time"

	"github.com/Multisend-ETH/go-multisend/multisendvy"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/sammy007/open-ethereum-pool/rpc"
	"github.com/sammy007/open-ethereum-pool/storage"
	"github.com/sammy007/open-ethereum-pool/util"
)

const txCheckInterval = 5 * time.Second

type PayoutsConfig struct {
	Enabled      bool   `json:"enabled"`
	RequirePeers int64  `json:"requirePeers"`
	Interval     string `json:"interval"`
	Daemon       string `json:"daemon"`
	Timeout      string `json:"timeout"`
	Address      string `json:"address"`
	Gas          string `json:"gas"`
	GasPrice     string `json:"gasPrice"`
	AutoGas      bool   `json:"autoGas"`
	// In Shannon
	Threshold        int64  `json:"threshold"`
	BgSave           bool   `json:"bgsave"`
	MultisendAddress string `json:"multisendAddress"`
}

func (self PayoutsConfig) GasHex() string {
	x := util.String2Big(self.Gas)
	return hexutil.EncodeBig(x)
}

func (self PayoutsConfig) GasPriceHex() string {
	x := util.String2Big(self.GasPrice)
	return hexutil.EncodeBig(x)
}

type PayoutsProcessor struct {
	config   *PayoutsConfig
	backend  *storage.RedisClient
	rpc      *rpc.RPCClient
	halt     bool
	lastFail error
}

type PayeeAmount struct {
	amountShanonInt64 int64
	amountShanon      *big.Int
	amountWei         *big.Int
}

func NewPayoutsProcessor(cfg *PayoutsConfig, backend *storage.RedisClient) *PayoutsProcessor {
	u := &PayoutsProcessor{config: cfg, backend: backend}
	u.rpc = rpc.NewRPCClient("PayoutsProcessor", cfg.Daemon, cfg.Timeout)
	return u
}

func (u *PayoutsProcessor) Start() {
	log.Println("Starting payouts")

	if u.mustResolvePayout() {
		log.Println("Running with env RESOLVE_PAYOUT=1, now trying to resolve locked payouts")
		u.resolvePayouts()
		log.Println("Now you have to restart payouts module with RESOLVE_PAYOUT=0 for normal run")
		return
	}

	intv := util.MustParseDuration(u.config.Interval)
	timer := time.NewTimer(intv)
	log.Printf("Set payouts interval to %v", intv)

	payments := u.backend.GetPendingPayments()
	if len(payments) > 0 {
		log.Printf("Previous payout failed, you have to resolve it. List of failed payments:\n %v",
			formatPendingPayments(payments))
		return
	}

	locked, err := u.backend.IsPayoutsLocked()
	if err != nil {
		log.Println("Unable to start payouts:", err)
		return
	}
	if locked {
		log.Println("Unable to start payouts because they are locked")
		return
	}

	// Immediately process payouts after start
	u.process()
	timer.Reset(intv)

	go func() {
		for {
			select {
			case <-timer.C:
				u.process()
				timer.Reset(intv)
			}
		}
	}()
}

func (u *PayoutsProcessor) process() {
	if u.halt {
		log.Println("Payments suspended due to last critical error:", u.lastFail)
		return
	}
	mustPay := 0
	minersPaid := 0
	totalAmount := big.NewInt(0)
	payees, err := u.backend.GetPayees()
	if err != nil {
		log.Println("Error while retrieving payees from backend:", err)
		return
	}

	if !u.checkPeers() {
		log.Println("Peers not connected, abort Payment!", err)
		return
	}

	if !u.isUnlockedAccount() {
		log.Println("Account is locked, abort payment!", err)
		return
	}

	poolBalance, err := u.rpc.GetBalance(u.config.Address)
	if err != nil {
		u.halt = true
		u.lastFail = err
		return
	}
	remainingPoolBalance := poolBalance

	validPayees := map[string]PayeeAmount{}

	addresses := []string{}
	amounts := []string{}
	batchIndex := 0
	multisendBatch := 100

	for _, login := range payees {
		amount, _ := u.backend.GetBalance(login)
		amountInShannon := big.NewInt(amount)

		// Shannon^2 = Wei
		amountInWei := new(big.Int).Mul(amountInShannon, util.Shannon)

		if !u.reachedThreshold(amountInShannon) {
			continue
		}
		mustPay++

		if remainingPoolBalance.Cmp(amountInWei) < 0 {
			err := fmt.Errorf("Not enough balance for payment, need %s Wei, pool has %s Wei",
				amountInWei.String(), poolBalance.String())
			u.halt = true
			u.lastFail = err
			break
		}

		remainingPoolBalance.Sub(remainingPoolBalance, amountInWei)

		// Lock payments for current payout
		err = u.backend.LockPayouts(login, amount)
		if err != nil {
			log.Printf("Failed to lock payment for %s: %v", login, err)
			u.halt = true
			u.lastFail = err
			break
		}
		log.Printf("Locked payment for %s, %v Shannon", login, amount)

		// Debit miner's balance and update stats
		err = u.backend.UpdateBalance(login, amount)
		if err != nil {
			log.Printf("Failed to update balance for %s, %v Shannon: %v", login, amount, err)
			u.halt = true
			u.lastFail = err
			break
		}

		payeeAmount := PayeeAmount{
			amountShanonInt64: amount,
			amountShanon:      amountInShannon,
			amountWei:         amountInWei,
		}

		validPayees[login] = payeeAmount
		addresses = append(addresses, login)
		amounts = append(amounts, amountInWei.String())

		if ((len(addresses))%multisendBatch) == 0 || len(addresses) == len(payees)-batchIndex*multisendBatch {

			_addresses := [100]string{}
			_amounts := [100]string{}
			copy(_addresses[:], addresses[:100])
			copy(_amounts[:], amounts[:100])

			callData := multisendvy.MultisendWeiData(context.Background(), _addresses, _amounts)
			data := &multisendvy.RPCSendETHTransactionCallData{
				From:     u.config.Address,
				To:       u.config.MultisendAddress,
				Value:    callData.Value.String(),
				Data:     hexutil.Encode(callData.Data),
				Gas:      u.config.Gas, //recommended: "3000000",
				GasPrice: u.config.GasPrice,
			}
			txHash, err := multisendvy.RPCSendETHTransaction(u.config.Daemon, data)
			if err != nil {
				log.Println("Failed to send batch transaction for batch: [", batchIndex+1, "] of ", validPayees)
				log.Println("Batch transaction error: ", err)
				u.halt = true
				u.lastFail = err
				break
			}
			log.Println("Multisend Transaction Hash: ", txHash)

			for _, address := range addresses {
				// log payment
				err = u.backend.WritePayment(address, txHash, validPayees[address].amountShanonInt64)
				if err != nil {
					log.Printf("Failed to log payment data for %s, %v Shannon, tx: %s: %v", address, amount, txHash, err)
					u.halt = true
					u.lastFail = err
					break
				}
				for {
					log.Printf("Waiting for tx confirmation: %v", txHash)
					time.Sleep(txCheckInterval)
					receipt, err := u.rpc.GetTxReceipt(txHash)
					if err != nil {
						log.Printf("Failed to get tx receipt for %v: %v", txHash, err)
						continue
					}
					// Tx has been mined
					if receipt != nil && receipt.Confirmed() {
						if receipt.Successful() {
							log.Printf("Payout tx successful for %s: %s", login, txHash)
						} else {
							log.Printf("Payout tx failed for %s: %s. Address contract throws on incoming tx.", login, txHash)
						}
						break
					}
				}
			}
			minersPaid += len(addresses)
			addresses = []string{}
			amounts = []string{}
			batchIndex++
		}

	}

	if mustPay > 0 {
		log.Printf("Paid total %v Shannon to %v of %v payees", totalAmount, minersPaid, mustPay)
	} else {
		log.Println("No payees that have reached payout threshold")
	}

	// Save redis state to disk
	if minersPaid > 0 && u.config.BgSave {
		u.bgSave()
	}
}

func (self PayoutsProcessor) isUnlockedAccount() bool {
	_, err := self.rpc.Sign(self.config.Address, "0x0")
	if err != nil {
		log.Println("Unable to process payouts:", err)
		return false
	}
	return true
}

func (self PayoutsProcessor) checkPeers() bool {
	n, err := self.rpc.GetPeerCount()
	if err != nil {
		log.Println("Unable to start payouts, failed to retrieve number of peers from node:", err)
		return false
	}
	if n < self.config.RequirePeers {
		log.Println("Unable to start payouts, number of peers on a node is less than required", self.config.RequirePeers)
		return false
	}
	return true
}

func (self PayoutsProcessor) reachedThreshold(amount *big.Int) bool {
	return big.NewInt(self.config.Threshold).Cmp(amount) < 0
}

func formatPendingPayments(list []*storage.PendingPayment) string {
	var s string
	for _, v := range list {
		s += fmt.Sprintf("\tAddress: %s, Amount: %v Shannon, %v\n", v.Address, v.Amount, time.Unix(v.Timestamp, 0))
	}
	return s
}

func (self PayoutsProcessor) bgSave() {
	result, err := self.backend.BgSave()
	if err != nil {
		log.Println("Failed to perform BGSAVE on backend:", err)
		return
	}
	log.Println("Saving backend state to disk:", result)
}

func (self PayoutsProcessor) resolvePayouts() {
	payments := self.backend.GetPendingPayments()

	if len(payments) > 0 {
		log.Printf("Will credit back following balances:\n%s", formatPendingPayments(payments))

		for _, v := range payments {
			err := self.backend.RollbackBalance(v.Address, v.Amount)
			if err != nil {
				log.Printf("Failed to credit %v Shannon back to %s, error is: %v", v.Amount, v.Address, err)
				return
			}
			log.Printf("Credited %v Shannon back to %s", v.Amount, v.Address)
		}
		err := self.backend.UnlockPayouts()
		if err != nil {
			log.Println("Failed to unlock payouts:", err)
			return
		}
	} else {
		log.Println("No pending payments to resolve")
	}

	if self.config.BgSave {
		self.bgSave()
	}
	log.Println("Payouts unlocked")
}

func (self PayoutsProcessor) mustResolvePayout() bool {
	v, _ := strconv.ParseBool(os.Getenv("RESOLVE_PAYOUT"))
	return v
}
