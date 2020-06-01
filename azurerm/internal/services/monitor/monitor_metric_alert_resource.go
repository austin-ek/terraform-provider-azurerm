package monitor

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/Azure/go-autorest/autorest/date"

	"github.com/Azure/azure-sdk-for-go/services/preview/monitor/mgmt/2019-06-01/insights"
	"github.com/hashicorp/go-azure-helpers/response"
	"github.com/hashicorp/terraform-plugin-sdk/helper/hashcode"
	"github.com/hashicorp/terraform-plugin-sdk/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/helper/validation"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/helpers/azure"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/helpers/tf"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/clients"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/features"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/location"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/tags"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/timeouts"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/utils"
)

func buildMonitorMetricAlertMultiMetricCriteriaSchema(extra map[string]*schema.Schema) map[string]*schema.Schema {
	base := map[string]*schema.Schema{
		"metric_namespace": {
			Type:         schema.TypeString,
			Required:     true,
			ValidateFunc: validation.StringIsNotEmpty,
		},
		"metric_name": {
			Type:         schema.TypeString,
			Required:     true,
			ValidateFunc: validation.StringIsNotEmpty,
		},
		"aggregation": {
			Type:     schema.TypeString,
			Required: true,
			ValidateFunc: validation.StringInSlice([]string{
				"Average",
				"Count",
				"Minimum",
				"Maximum",
				"Total",
			}, false),
		},
		"dimension": {
			Type:     schema.TypeList,
			Optional: true,
			Elem: &schema.Resource{
				Schema: map[string]*schema.Schema{
					"name": {
						Type:         schema.TypeString,
						Required:     true,
						ValidateFunc: validation.StringIsNotEmpty,
					},
					"operator": {
						Type:     schema.TypeString,
						Required: true,
						ValidateFunc: validation.StringInSlice([]string{
							"Include",
							"Exclude",
						}, false),
					},
					"values": {
						Type:     schema.TypeList,
						Required: true,
						MinItems: 1,
						Elem: &schema.Schema{
							Type: schema.TypeString,
						},
					},
				},
			},
		},
	}
	for k, v := range extra {
		base[k] = v
	}
	return base
}

var (
	monitorMetricAlertStaticMetricCriteriaSchema = buildMonitorMetricAlertMultiMetricCriteriaSchema(
		map[string]*schema.Schema{
			"operator": {
				Type:     schema.TypeString,
				Required: true,
				ValidateFunc: validation.StringInSlice([]string{
					string(insights.OperatorEquals),
					string(insights.OperatorGreaterThan),
					string(insights.OperatorGreaterThanOrEqual),
					string(insights.OperatorLessThan),
					string(insights.OperatorLessThanOrEqual),
					string(insights.OperatorNotEquals),
				}, false),
			},
			"threshold": {
				Type:     schema.TypeFloat,
				Required: true,
			},
		},
	)
	monitorMetricAlertDynamicMetricCriteriaSchema = buildMonitorMetricAlertMultiMetricCriteriaSchema(
		map[string]*schema.Schema{
			"operator": {
				Type:     schema.TypeString,
				Required: true,
				ValidateFunc: validation.StringInSlice([]string{
					string(insights.DynamicThresholdOperatorLessThan),
					string(insights.DynamicThresholdOperatorGreaterThan),
					string(insights.DynamicThresholdOperatorGreaterOrLessThan),
				}, false),
			},
			"alert_sensitivity": {
				Type:     schema.TypeString,
				Required: true,
				ValidateFunc: validation.StringInSlice([]string{
					string(insights.Low),
					string(insights.Medium),
					string(insights.High),
				}, false),
			},

			"evaluation_total_count": {
				Type:         schema.TypeInt,
				Optional:     true,
				ValidateFunc: validation.IntAtLeast(1),
				Default:      4,
			},

			"evaluation_failure_count": {
				Type:         schema.TypeInt,
				Optional:     true,
				ValidateFunc: validation.IntAtLeast(1),
				Default:      4,
			},

			"ignore_data_before": {
				Type:         schema.TypeString,
				Optional:     true,
				ValidateFunc: validation.IsRFC3339Time,
			},
		},
	)
)

func resourceArmMonitorMetricAlert() *schema.Resource {
	return &schema.Resource{
		Create: resourceArmMonitorMetricAlertCreateUpdate,
		Read:   resourceArmMonitorMetricAlertRead,
		Update: resourceArmMonitorMetricAlertCreateUpdate,
		Delete: resourceArmMonitorMetricAlertDelete,

		Importer: &schema.ResourceImporter{
			State: schema.ImportStatePassthrough,
		},

		Timeouts: &schema.ResourceTimeout{
			Create: schema.DefaultTimeout(30 * time.Minute),
			Read:   schema.DefaultTimeout(5 * time.Minute),
			Update: schema.DefaultTimeout(30 * time.Minute),
			Delete: schema.DefaultTimeout(30 * time.Minute),
		},

		Schema: map[string]*schema.Schema{
			"name": {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validation.StringIsNotEmpty,
			},

			"resource_group_name": azure.SchemaResourceGroupName(),

			"scopes": {
				Type:     schema.TypeSet,
				Required: true,
				MinItems: 1,
				Elem: &schema.Schema{
					Type:         schema.TypeString,
					ValidateFunc: azure.ValidateResourceID,
				},
				Set: schema.HashString,
			},

			"target_resource_type": {
				Type:        schema.TypeString,
				Optional:    true,
				Computed:    true,
				Description: `The resource type (e.g. Microsoft.Compute/virtualMachines) of the target resource. Required when using subscription, resource group scope or multiple scopes.`,
			},

			"target_resource_location": {
				Type:             schema.TypeString,
				Optional:         true,
				Computed:         true,
				StateFunc:        location.StateFunc,
				DiffSuppressFunc: location.DiffSuppressFunc,
				Description:      `The location of the target resource. Required when using subscription, resource group scope or multiple scopes.`,
			},

			// static criteria
			"criteria": {
				Type:         schema.TypeSet,
				Optional:     true,
				MinItems:     1,
				ExactlyOneOf: []string{"dynamic_criteria", "webtest_location_availability_criteria"},
				Elem: &schema.Resource{
					Schema: monitorMetricAlertStaticMetricCriteriaSchema,
				},
			},

			"dynamic_criteria": {
				Type:     schema.TypeSet,
				Optional: true,
				MinItems: 1,
				// Curently, it allows to define only one dynamic criteria in one metric alert.
				MaxItems:     1,
				ExactlyOneOf: []string{"criteria", "webtest_location_availability_criteria"},
				Elem: &schema.Resource{
					Schema: monitorMetricAlertDynamicMetricCriteriaSchema,
				},
			},

			"webtest_location_availability_criteria": {
				Type:         schema.TypeList,
				Optional:     true,
				MinItems:     1,
				MaxItems:     1,
				ExactlyOneOf: []string{"criteria", "dynamic_criteria"},
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"webtest_id": {
							Type:         schema.TypeString,
							Required:     true,
							ValidateFunc: azure.ValidateResourceID,
						},
						"component_id": {
							Type:         schema.TypeString,
							Required:     true,
							ValidateFunc: azure.ValidateResourceID,
						},
						"failed_location_count": {
							Type:         schema.TypeInt,
							Required:     true,
							ValidateFunc: validation.IntAtLeast(1),
						},
					},
				},
			},

			"action": {
				Type:     schema.TypeSet,
				Optional: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"action_group_id": {
							Type:         schema.TypeString,
							Required:     true,
							ValidateFunc: azure.ValidateResourceID,
						},
						"webhook_properties": {
							Type:     schema.TypeMap,
							Optional: true,
							Elem: &schema.Schema{
								Type: schema.TypeString,
							},
						},
					},
				},
				Set: resourceArmMonitorMetricAlertActionHash,
			},

			"auto_mitigate": {
				Type:     schema.TypeBool,
				Optional: true,
				Default:  true,
			},

			"description": {
				Type:     schema.TypeString,
				Optional: true,
			},

			"enabled": {
				Type:     schema.TypeBool,
				Optional: true,
				Default:  true,
			},

			"frequency": {
				Type:     schema.TypeString,
				Optional: true,
				Default:  "PT1M",
				ValidateFunc: validation.StringInSlice([]string{
					"PT1M",
					"PT5M",
					"PT15M",
					"PT30M",
					"PT1H",
				}, false),
			},

			"severity": {
				Type:         schema.TypeInt,
				Optional:     true,
				Default:      3,
				ValidateFunc: validation.IntBetween(0, 4),
			},

			"window_size": {
				Type:     schema.TypeString,
				Optional: true,
				Default:  "PT5M",
				ValidateFunc: validation.StringInSlice([]string{
					"PT1M",
					"PT5M",
					"PT15M",
					"PT30M",
					"PT1H",
					"PT6H",
					"PT12H",
					"P1D",
				}, false),
			},

			"tags": tags.Schema(),
		},
	}
}

func resourceArmMonitorMetricAlertCreateUpdate(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Monitor.MetricAlertsClient
	ctx, cancel := timeouts.ForCreateUpdate(meta.(*clients.Client).StopContext, d)
	defer cancel()

	name := d.Get("name").(string)
	resourceGroup := d.Get("resource_group_name").(string)

	if features.ShouldResourcesBeImported() && d.IsNewResource() {
		existing, err := client.Get(ctx, resourceGroup, name)
		if err != nil {
			if !utils.ResponseWasNotFound(existing.Response) {
				return fmt.Errorf("Error checking for presence of existing Monitor Metric Alert %q (Resource Group %q): %s", name, resourceGroup, err)
			}
		}

		if existing.ID != nil && *existing.ID != "" {
			return tf.ImportAsExistsError("azurerm_monitor_metric_alert", *existing.ID)
		}
	}

	enabled := d.Get("enabled").(bool)
	autoMitigate := d.Get("auto_mitigate").(bool)
	description := d.Get("description").(string)
	scopesRaw := d.Get("scopes").(*schema.Set).List()
	severity := d.Get("severity").(int)
	frequency := d.Get("frequency").(string)
	windowSize := d.Get("window_size").(string)
	actionRaw := d.Get("action").(*schema.Set).List()
	targetResourceType := d.Get("target_resource_type").(string)
	targetResourceLocation := d.Get("target_resource_location").(string)

	t := d.Get("tags").(map[string]interface{})
	expandedTags := tags.Expand(t)

	parameters := insights.MetricAlertResource{
		Location: utils.String(azure.NormalizeLocation("Global")),
		MetricAlertProperties: &insights.MetricAlertProperties{
			Enabled:              utils.Bool(enabled),
			AutoMitigate:         utils.Bool(autoMitigate),
			Description:          utils.String(description),
			Severity:             utils.Int32(int32(severity)),
			EvaluationFrequency:  utils.String(frequency),
			WindowSize:           utils.String(windowSize),
			Scopes:               utils.ExpandStringSlice(scopesRaw),
			Criteria:             expandMonitorMetricAlertCriteria(d),
			Actions:              expandMonitorMetricAlertAction(actionRaw),
			TargetResourceType:   utils.String(targetResourceType),
			TargetResourceRegion: utils.String(targetResourceLocation),
		},
		Tags: expandedTags,
	}

	if _, err := client.CreateOrUpdate(ctx, resourceGroup, name, parameters); err != nil {
		return fmt.Errorf("Error creating or updating metric alert %q (resource group %q): %+v", name, resourceGroup, err)
	}

	read, err := client.Get(ctx, resourceGroup, name)
	if err != nil {
		return err
	}
	if read.ID == nil {
		return fmt.Errorf("Metric alert %q (resource group %q) ID is empty", name, resourceGroup)
	}
	d.SetId(*read.ID)

	return resourceArmMonitorMetricAlertRead(d, meta)
}

func resourceArmMonitorMetricAlertRead(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Monitor.MetricAlertsClient
	ctx, cancel := timeouts.ForRead(meta.(*clients.Client).StopContext, d)
	defer cancel()

	id, err := azure.ParseAzureResourceID(d.Id())
	if err != nil {
		return err
	}
	resourceGroup := id.ResourceGroup
	name := id.Path["metricAlerts"]

	resp, err := client.Get(ctx, resourceGroup, name)
	if err != nil {
		if utils.ResponseWasNotFound(resp.Response) {
			log.Printf("[DEBUG] Metric Alert %q was not found in Resource Group %q - removing from state!", name, resourceGroup)
			d.SetId("")
			return nil
		}
		return fmt.Errorf("Error getting metric alert %q (resource group %q): %+v", name, resourceGroup, err)
	}

	d.Set("name", name)
	d.Set("resource_group_name", resourceGroup)
	if alert := resp.MetricAlertProperties; alert != nil {
		d.Set("enabled", alert.Enabled)
		d.Set("auto_mitigate", alert.AutoMitigate)
		d.Set("description", alert.Description)
		d.Set("severity", alert.Severity)
		d.Set("frequency", alert.EvaluationFrequency)
		d.Set("window_size", alert.WindowSize)
		if err := d.Set("scopes", utils.FlattenStringSlice(alert.Scopes)); err != nil {
			return fmt.Errorf("Error setting `scopes`: %+v", err)
		}

		// Determine the correct criteria schema to set
		var criteriaSchema string
		switch c := alert.Criteria.(type) {
		case insights.MetricAlertSingleResourceMultipleMetricCriteria:
			criteriaSchema = "criteria"
		case insights.MetricAlertMultipleResourceMultipleMetricCriteria:
			if c.AllOf == nil || len(*c.AllOf) == 0 {
				return errors.New("nil or empty contained criteria of MultipleResourceMultipleMetricCriteria")
			}
			// `MinItems` defined in schema guaranteed there is at least one element.
			switch (*c.AllOf)[0].(type) {
			case insights.DynamicMetricCriteria:
				criteriaSchema = "dynamic_criteria"
			case insights.MetricCriteria:
				criteriaSchema = "criteria"
			}
		case insights.WebtestLocationAvailabilityCriteria:
			criteriaSchema = "webtest_location_availability_criteria"
		}
		// lintignore:R001
		if err := d.Set(criteriaSchema, flattenMonitorMetricAlertCriteria(alert.Criteria)); err != nil {
			return fmt.Errorf("Error setting `%s`: %+v", criteriaSchema, err)
		}

		if err := d.Set("action", flattenMonitorMetricAlertAction(alert.Actions)); err != nil {
			return fmt.Errorf("Error setting `action`: %+v", err)
		}
		d.Set("target_resource_type", alert.TargetResourceType)
		d.Set("target_resource_location", alert.TargetResourceRegion)
	}
	return tags.FlattenAndSet(d, resp.Tags)
}

func resourceArmMonitorMetricAlertDelete(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Monitor.MetricAlertsClient
	ctx, cancel := timeouts.ForDelete(meta.(*clients.Client).StopContext, d)
	defer cancel()

	id, err := azure.ParseAzureResourceID(d.Id())
	if err != nil {
		return err
	}
	resourceGroup := id.ResourceGroup
	name := id.Path["metricAlerts"]

	if resp, err := client.Delete(ctx, resourceGroup, name); err != nil {
		if !response.WasNotFound(resp.Response) {
			return fmt.Errorf("Error deleting metric alert %q (resource group %q): %+v", name, resourceGroup, err)
		}
	}

	return nil
}

func expandMonitorMetricAlertCriteria(d *schema.ResourceData) insights.BasicMetricAlertCriteria {
	switch {
	case d.Get("criteria").(*schema.Set).Len() != 0:
		return expandMonitorMetricAlertMetricCriteria(d.Get("criteria").(*schema.Set).List())
	case d.Get("dynamic_criteria").(*schema.Set).Len() != 0:
		return expandMonitorMetricAlertDynamicMetricCriteria(d.Get("dynamic_criteria").(*schema.Set).List())
	case d.Get("webtest_location_availability_criteria").(*schema.Set).Len() != 0:
		return expandMonitorMetricAlertWebtestLocAvailCriteria(d.Get("webtest_location_availability_criteria").([]interface{}))
	default:
		// Guaranteed by schema `AtLeastOne` constraint
		panic("will never happen")
	}
}

func expandMonitorMetricAlertMetricCriteria(input []interface{}) insights.BasicMetricAlertCriteria {
	criteria := make([]insights.BasicMultiMetricCriteria, 0)
	for i, item := range input {
		v := item.(map[string]interface{})
		dimensions := expandMonitorMetricAlertMultiMetricCriteriaDimension(v["dimension"].([]interface{}))
		criteria = append(criteria, insights.MetricCriteria{
			Name:            utils.String(fmt.Sprintf("Metric%d", i+1)),
			MetricNamespace: utils.String(v["metric_namespace"].(string)),
			MetricName:      utils.String(v["metric_name"].(string)),
			TimeAggregation: v["aggregation"].(string),
			Dimensions:      &dimensions,
			Operator:        insights.Operator(v["operator"].(string)),
			Threshold:       utils.Float(v["threshold"].(float64)),
		})
	}
	return &insights.MetricAlertMultipleResourceMultipleMetricCriteria{
		AllOf:     &criteria,
		OdataType: insights.OdataTypeMicrosoftAzureMonitorMultipleResourceMultipleMetricCriteria,
	}
}
func expandMonitorMetricAlertDynamicMetricCriteria(input []interface{}) insights.BasicMetricAlertCriteria {
	criteria := make([]insights.BasicMultiMetricCriteria, 0)
	for i, item := range input {
		v := item.(map[string]interface{})
		dimensions := expandMonitorMetricAlertMultiMetricCriteriaDimension(v["dimension"].([]interface{}))
		var ignoreDataBefore *date.Time
		if v := v["ignore_data_before"].(string); v != "" {
			// Guaranteed in schema validation func.
			t, _ := time.Parse(time.RFC3339, v)
			ignoreDataBefore = &date.Time{Time: t}
		}
		criteria = append(criteria, insights.DynamicMetricCriteria{
			Name:             utils.String(fmt.Sprintf("Metric%d", i+1)),
			MetricNamespace:  utils.String(v["metric_namespace"].(string)),
			MetricName:       utils.String(v["metric_name"].(string)),
			TimeAggregation:  v["aggregation"].(string),
			Dimensions:       &dimensions,
			Operator:         insights.DynamicThresholdOperator(v["operator"].(string)),
			AlertSensitivity: insights.DynamicThresholdSensitivity(v["alert_sensitivity"].(string)),
			FailingPeriods: &insights.DynamicThresholdFailingPeriods{
				NumberOfEvaluationPeriods: utils.Float(float64(v["evaluation_total_count"].(int))),
				MinFailingPeriodsToAlert:  utils.Float(float64(v["evaluation_failure_count"].(int))),
			},
			IgnoreDataBefore: ignoreDataBefore,
		})
	}
	return &insights.MetricAlertMultipleResourceMultipleMetricCriteria{
		AllOf:     &criteria,
		OdataType: insights.OdataTypeMicrosoftAzureMonitorMultipleResourceMultipleMetricCriteria,
	}
}

func expandMonitorMetricAlertWebtestLocAvailCriteria(input []interface{}) insights.BasicMetricAlertCriteria {
	if len(input) == 0 {
		return nil
	}
	v := input[0].(map[string]interface{})
	return &insights.WebtestLocationAvailabilityCriteria{
		WebTestID:           utils.String(v["webtest_id"].(string)),
		ComponentID:         utils.String(v["component_id"].(string)),
		FailedLocationCount: utils.Float(float64(v["failed_location_count"].(int))),
	}
}

func expandMonitorMetricAlertMultiMetricCriteriaDimension(input []interface{}) []insights.MetricDimension {
	result := make([]insights.MetricDimension, 0)
	for _, dimension := range input {
		dVal := dimension.(map[string]interface{})
		result = append(result, insights.MetricDimension{
			Name:     utils.String(dVal["name"].(string)),
			Operator: utils.String(dVal["operator"].(string)),
			Values:   utils.ExpandStringSlice(dVal["values"].([]interface{})),
		})
	}
	return result
}

func expandMonitorMetricAlertAction(input []interface{}) *[]insights.MetricAlertAction {
	actions := make([]insights.MetricAlertAction, 0)
	for _, item := range input {
		v := item.(map[string]interface{})
		if agID := v["action_group_id"].(string); agID != "" {
			props := make(map[string]*string)
			if pVal, ok := v["webhook_properties"]; ok {
				for pk, pv := range pVal.(map[string]interface{}) {
					props[pk] = utils.String(pv.(string))
				}
			}

			actions = append(actions, insights.MetricAlertAction{
				ActionGroupID:     utils.String(agID),
				WebHookProperties: props,
			})
		}
	}
	return &actions
}

func flattenMonitorMetricAlertCriteria(input insights.BasicMetricAlertCriteria) (result []interface{}) {
	switch criteria := input.(type) {
	case insights.MetricAlertSingleResourceMultipleMetricCriteria:
		// As service is gonna deprecate data type of `MetricAlertSingleResourceMultipleMetricCriteria`,
		// we use the same function to handle both single/multiple ResourceMultipleMetricCriteria cases.
		// This is possible because their `AllOf` member implement the same interface: `MultiMetricCriteria`.
		if criteria.AllOf == nil {
			return nil
		}
		l := make([]insights.BasicMultiMetricCriteria, len(*criteria.AllOf))
		for i, c := range *criteria.AllOf {
			l[i] = c
		}
		return flattenMonitorMetricAlertMultiMetricCriteria(&l)
	case insights.MetricAlertMultipleResourceMultipleMetricCriteria:
		return flattenMonitorMetricAlertMultiMetricCriteria(criteria.AllOf)
	case insights.WebtestLocationAvailabilityCriteria:
		return flattenMonitorMetricAlertWebtestLocAvailCriteria(&criteria)
	default:
		return nil
	}
}

func flattenMonitorMetricAlertMultiMetricCriteria(input *[]insights.BasicMultiMetricCriteria) []interface{} {
	if input == nil {
		return nil
	}
	result := make([]interface{}, 0)

	for _, criteria := range *input {
		v := make(map[string]interface{})
		var (
			metricName      string
			metricNamespace string
			timeAggregation interface{}
			dimensions      []insights.MetricDimension
		)

		switch criteria := criteria.(type) {
		case insights.MetricCriteria:
			if criteria.MetricName != nil {
				metricName = *criteria.MetricName
			}
			if criteria.MetricNamespace != nil {
				metricNamespace = *criteria.MetricNamespace
			}
			timeAggregation = criteria.TimeAggregation
			if criteria.Dimensions != nil {
				dimensions = *criteria.Dimensions
			}

			// MetricCriteria specific properties
			v["operator"] = string(criteria.Operator)

			threshold := 0.0
			if criteria.Threshold != nil {
				threshold = *criteria.Threshold
			}
			v["threshold"] = threshold
		case insights.DynamicMetricCriteria:
			if criteria.MetricName != nil {
				metricName = *criteria.MetricName
			}
			if criteria.MetricNamespace != nil {
				metricNamespace = *criteria.MetricNamespace
			}
			timeAggregation = criteria.TimeAggregation
			if criteria.Dimensions != nil {
				dimensions = *criteria.Dimensions
			}
			// DynamicMetricCriteria specific properties
			v["operator"] = string(criteria.Operator)
			v["alert_sensitivity"] = string(criteria.AlertSensitivity)
			var (
				nEvl     = 1
				nFailEvl = 1
			)
			if period := criteria.FailingPeriods; period != nil {
				if period.NumberOfEvaluationPeriods != nil {
					nEvl = int(*period.NumberOfEvaluationPeriods)
				}
				if period.MinFailingPeriodsToAlert != nil {
					nFailEvl = int(*period.MinFailingPeriodsToAlert)
				}
			}
			v["evaluation_total_count"] = nEvl
			v["evaluation_failure_count"] = nFailEvl

			ignoreDataBefore := ""
			if criteria.IgnoreDataBefore != nil {
				ignoreDataBefore = criteria.IgnoreDataBefore.Format(time.RFC3339)
			}
			v["ignore_data_before"] = ignoreDataBefore
		}

		// Common properties
		v["metric_name"] = metricName
		v["metric_namespace"] = metricNamespace
		v["aggregation"] = timeAggregation
		if dimensions != nil {
			dimResult := make([]map[string]interface{}, 0)
			for _, dimension := range dimensions {
				dVal := make(map[string]interface{})
				if dimension.Name != nil {
					dVal["name"] = *dimension.Name
				}
				if dimension.Operator != nil {
					dVal["operator"] = *dimension.Operator
				}
				dVal["values"] = utils.FlattenStringSlice(dimension.Values)
				dimResult = append(dimResult, dVal)
			}
			v["dimension"] = dimResult
		}

		result = append(result, v)
	}
	return result
}

func flattenMonitorMetricAlertWebtestLocAvailCriteria(input *insights.WebtestLocationAvailabilityCriteria) []interface{} {
	if input == nil {
		return nil
	}
	webtestID := ""
	if input.WebTestID != nil {
		webtestID = *input.WebTestID
	}

	componentID := ""
	if input.ComponentID != nil {
		componentID = *input.ComponentID
	}

	failedLocationCount := 0
	if input.FailedLocationCount != nil {
		failedLocationCount = int(*input.FailedLocationCount)
	}

	return []interface{}{
		map[string]interface{}{
			"webtest_id":            webtestID,
			"component_id":          componentID,
			"failed_location_count": failedLocationCount,
		},
	}
}

func flattenMonitorMetricAlertAction(input *[]insights.MetricAlertAction) (result []interface{}) {
	result = make([]interface{}, 0)
	if input == nil {
		return
	}
	for _, action := range *input {
		v := make(map[string]interface{})

		if action.ActionGroupID != nil {
			v["action_group_id"] = *action.ActionGroupID
		}

		props := make(map[string]string)
		for pk, pv := range action.WebHookProperties {
			if pv != nil {
				props[pk] = *pv
			}
		}
		v["webhook_properties"] = props

		result = append(result, v)
	}

	return result
}

func resourceArmMonitorMetricAlertActionHash(input interface{}) int {
	var buf bytes.Buffer
	if v, ok := input.(map[string]interface{}); ok {
		buf.WriteString(fmt.Sprintf("%s-", v["action_group_id"].(string)))
	}
	return hashcode.String(buf.String())
}
