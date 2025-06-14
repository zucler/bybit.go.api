package main

import (
	"context"
	"fmt"

	bybit "github.com/zucler/bybit.go.api"
)

func main() {
	GetFeeRates()
}

func GetFeeRates() {
	client := bybit.NewBybitHttpClient("YOUR_API_KEY", "YOUR_API_SECRET", bybit.WithBaseURL(bybit.TESTNET))
	params := map[string]interface{}{"symbol": "BTCUSDT", "category": "linear"}
	accountResult, err := client.NewUtaBybitServiceWithParams(params).GetFeeRates(context.Background())
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(bybit.PrettyPrint(accountResult))
}
