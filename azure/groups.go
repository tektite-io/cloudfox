package azure

import (
	"context"
	"fmt"

	"github.com/BishopFox/cloudfox/utils"
	"github.com/aws/smithy-go/ptr"
)

var ListSubscriptions = listSubscriptions

func listSubscriptions() ([]string, error) {
	var subs []string
	subsClient, err := utils.GetSubscriptionsClient()
	if err != nil {
		return subs, err
	}
	for page, err := subsClient.List(context.TODO()); page.NotDone(); err = page.Next() {
		if err != nil {
			return subs, err
		}
		for _, sub := range page.Values() {
			subs = append(subs, ptr.ToString(sub.SubscriptionID))
		}
	}
	return subs, nil
}

var ListResourceGroups = listResourceGroups

func listResourceGroups(subscription string) ([]string, error) {
	var resourceGroups []string
	rgClient, err := utils.GetResourceGroupsClient(subscription)
	if err != nil {
		return resourceGroups, err
	}
	for page, err := rgClient.List(context.TODO(), "", nil); page.NotDone(); err = page.Next() {
		if err != nil {
			return resourceGroups, err
		}
		for _, rg := range page.Values() {
			if rg.Name != nil {
				resourceGroups = append(resourceGroups, ptr.ToString(rg.Name))
			}
		}
	}
	return resourceGroups, nil
}

func PrintAvailableScope() (map[int]string, error) {
	fmt.Println("Fetching available resource groups from your Azure CLI session...")
	fmt.Println()

	var index int
	menu := make(map[int]string)

	subs, err := ListSubscriptions()
	if err != nil {
		return menu, err
	}
	for _, sub := range subs {
		fmt.Printf("Subscription: %s\n", sub)
		rgs, err := ListResourceGroups(sub)
		if err != nil {
			return menu, err
		}
		for _, rg := range rgs {
			index++
			fmt.Printf("[%d] RG: %s\n", index, rg)
			menu[index] = rg
		}
	}
	return menu, nil
}
