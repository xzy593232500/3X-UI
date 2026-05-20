package job

import (
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/web/service"
)

type CustomerSubscriptionExpiryJob struct {
	subscriptionMarket service.SubscriptionMarketService
	xrayService        service.XrayService
}

func NewCustomerSubscriptionExpiryJob() *CustomerSubscriptionExpiryJob {
	return new(CustomerSubscriptionExpiryJob)
}

func (j *CustomerSubscriptionExpiryJob) Run() {
	if !service.SubscriptionRelayEnabled() {
		return
	}
	count, err := j.subscriptionMarket.DisableExpiredCustomers()
	if err != nil {
		logger.Warning("disable expired customer subscriptions failed:", err)
		return
	}
	if count > 0 {
		j.xrayService.SetToNeedRestart()
	}
}
