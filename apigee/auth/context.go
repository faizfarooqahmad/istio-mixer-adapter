package auth

import (
	"time"
	"github.com/apigee/istio-mixer-adapter/apigee/context"
	"encoding/json"
	"strconv"
	"fmt"
)

type Context struct {
	context.Context
	ClientID       string
	AccessToken    string
	Application    string
	APIProducts    []string
	Expires        time.Time
	DeveloperEmail string
	Scopes         []string
}

// does nothing if claims is empty
func (a *Context) setClaims(claims map[string]interface{}) error {
	// todo: I'm not certain how Istio formats these claims values...

	a.Log().Infof("setClaims: %v", claims)

	if claims["client_id"] == nil {
		return nil
	}

	products, err := parseArrayOfStrings(claims["api_product_list"])
	if err != nil {
		return fmt.Errorf("unable to interpret api_product_list: %v", claims["api_product_list"])
	}

	scopes, err := parseArrayOfStrings(claims["scopes"])
	if err != nil {
		return fmt.Errorf("unable to interpret scopes: %v", claims["scopes"])
	}

	exp, ok := claims["exp"].(float64)
	if !ok {
		if str, ok := claims["exp"].(string); ok {
			var err error
			if exp, err = strconv.ParseFloat(str, 64); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("unable to interpret exp: %v", claims["exp"])
		}
	}
	a.Log().Infof("exp: %v", exp)

	if a.ClientID, ok = claims["client_id"].(string); !ok {
		return fmt.Errorf("unable to interpret client_id: %v", claims["client_id"])
	}
	//if a.AccessToken, ok = claims["access_token"].(string); !ok {
	//	return fmt.Errorf("unable to interpret access_token: %v", claims["access_token"])
	//}
	if a.Application, ok = claims["application_name"].(string); !ok {
		return fmt.Errorf("unable to interpret application_name: %v", claims["application_name"])
	}
	a.APIProducts = products
	a.Scopes = scopes
	a.Expires = time.Unix(int64(exp), 0)

	a.Log().Infof("claims set: %v", a)
	return nil
}

func parseArrayOfStrings(obj interface{}) (results []string, err error) {
	if arr, ok := obj.([]interface{}); ok {
		for _, unk := range arr {
			if obj, ok := unk.(string); ok {
				results = append(results, obj)
			} else {
				err = fmt.Errorf("unable to interpret: %v", unk)
				break
			}
		}
		return results, err
	} else if str, ok := obj.(string); ok {
		err = json.Unmarshal([]byte(str), &results)
		return
	}
	return
}

// pulls from jwt claims if available, api key if not
func Authenticate(ctx context.Context, apiKey string, claims map[string]interface{}) (Context, error) {

	ctx.Log().Infof("Authenticate: key: %v, claims: %v", apiKey, claims)

	var ac = Context{Context: ctx}
	err := ac.setClaims(claims)
	if ac.ClientID != "" || err != nil {
		return ac, err
	}

	if apiKey == "" {
		return ac, fmt.Errorf("missing api key")
	}

	// todo: cache apiKey => jwt

	claims, err = VerifyAPIKey(ctx, apiKey)
	if err != nil {
		return ac, err
	}

	err = ac.setClaims(claims)

	ctx.Log().Infof("Authenticate complete: %v [%v]", ac, err)
	return ac, err
}

// todo: add developerEmail
/*
jwt claims:
{
 api_product_list: [
  "EdgeMicroTestProduct"
 ],
 audience: "microgateway",
 jti: "29e2320b-787c-4625-8599-acc5e05c68d0",
 iss: "https://theganyo1-eval-test.apigee.net/edgemicro-auth/token",
 access_token: "8E7Az3ZgPHKrgzcQA54qAzXT3Z1G",
 client_id: "yBQ5eXZA8rSoipYEi1Rmn0Z8RKtkGI4H",
 nbf: 1516387728,
 iat: 1516387728,
 application_name: "61cd4d83-06b5-4270-a9ee-cf9255ef45c3",
 scopes: [
  "scope1",
  "scope2"
 ],
 exp: 1516388028
}
 */
