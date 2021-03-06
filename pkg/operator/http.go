package operator

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	api "github.com/operator-framework/operator-metering/pkg/apis/metering/v1alpha1"
	cbutil "github.com/operator-framework/operator-metering/pkg/apis/metering/v1alpha1/util"
	listers "github.com/operator-framework/operator-metering/pkg/generated/listers/metering/v1alpha1"
	"github.com/operator-framework/operator-metering/pkg/operator/prestostore"
	"github.com/operator-framework/operator-metering/pkg/operator/reportingutil"
	"github.com/operator-framework/operator-metering/pkg/presto"
	"github.com/operator-framework/operator-metering/pkg/util/chiprometheus"
	"github.com/operator-framework/operator-metering/pkg/util/orderedmap"
)

var ErrReportIsRunning = errors.New("the report is still running")
var prometheusMiddleware = chiprometheus.NewMiddleware("reporting-operator")

const (
	APIV1ReportsGetEndpoint    = "/api/v1/reports/get"
	APIV2ReportsEndpointPrefix = "/api/v2/reports"
)

type server struct {
	logger log.FieldLogger

	rand          *rand.Rand
	collectorFunc prometheusImporterFunc

	prometheusMetricsRepo prestostore.PrometheusMetricsRepo
	reportResultsGetter   prestostore.ReportResultsGetter

	reportLister                 listers.ReportLister
	reportGenerationQuerieLister listers.ReportGenerationQueryLister
	prestoTableLister            listers.PrestoTableLister
}

type requestLogger struct {
	log.FieldLogger
}

func (l *requestLogger) Print(v ...interface{}) {
	l.FieldLogger.Info(v...)
}

func newRouter(
	logger log.FieldLogger,
	rand *rand.Rand,
	prometheusMetricsRepo prestostore.PrometheusMetricsRepo,
	reportResultsGetter prestostore.ReportResultsGetter,
	collectorFunc prometheusImporterFunc,
	reportLister listers.ReportLister,
	reportGenerationQuerieLister listers.ReportGenerationQueryLister,
	prestoTableLister listers.PrestoTableLister,
) chi.Router {
	router := chi.NewRouter()
	logger = logger.WithField("component", "api")
	requestLogger := middleware.RequestLogger(&middleware.DefaultLogFormatter{Logger: &requestLogger{logger}})
	router.Use(requestLogger)
	router.Use(prometheusMiddleware)

	srv := &server{
		logger:                       logger,
		rand:                         rand,
		collectorFunc:                collectorFunc,
		prometheusMetricsRepo:        prometheusMetricsRepo,
		reportResultsGetter:          reportResultsGetter,
		reportLister:                 reportLister,
		reportGenerationQuerieLister: reportGenerationQuerieLister,
		prestoTableLister:            prestoTableLister,
	}

	router.HandleFunc(APIV2ReportsEndpointPrefix+"/{namespace}/{name}/full", srv.getReportV2FullHandler)
	router.HandleFunc(APIV2ReportsEndpointPrefix+"/{namespace}/{name}/table", srv.getReportV2TableHandler)
	router.HandleFunc(APIV1ReportsGetEndpoint, srv.getReportV1Handler)
	router.HandleFunc("/api/v1/datasources/prometheus/collect/{namespace}", srv.collectPromsumDataHandler)
	router.HandleFunc("/api/v1/datasources/prometheus/collect/{namespace}/{datasourceName}", srv.collectPromsumDataHandler)
	router.HandleFunc("/api/v1/datasources/prometheus/store/{namespace}/{datasourceName}", srv.storePromsumDataHandler)
	router.HandleFunc("/api/v1/datasources/prometheus/fetch/{namespace}/{datasourceName}", srv.fetchPromsumDataHandler)

	return router
}

func (srv *server) validateGetReportReq(logger log.FieldLogger, requiredQueryParams []string, w http.ResponseWriter, r *http.Request) bool {
	if r.Method != "GET" {
		writeErrorResponse(logger, w, r, http.StatusNotFound, "Not found")
		return false
	}
	err := r.ParseForm()
	if err != nil {
		writeErrorResponse(logger, w, r, http.StatusBadRequest, "couldn't parse URL query params: %v", err)
		return false
	}
	err = checkForFields(requiredQueryParams, r.Form)
	if err != nil {
		writeErrorResponse(logger, w, r, http.StatusBadRequest, "%v", err)
		return false
	}
	format := r.Form["format"][0]
	switch format {
	case "json", "csv", "tab", "tabular":
		return true
	}
	writeErrorResponse(logger, w, r, http.StatusBadRequest, "format must be one of: csv, json or tabular")
	return false
}

func (srv *server) getReportV1Handler(w http.ResponseWriter, r *http.Request) {
	logger := newRequestLogger(srv.logger, r, srv.rand)
	if !srv.validateGetReportReq(logger, []string{"name", "namespace", "format"}, w, r) {
		return
	}
	srv.getReport(logger, r.Form["name"][0], r.Form["namespace"][0], r.Form["format"][0], false, true, w, r)
}

func (srv *server) getReportV2FullHandler(w http.ResponseWriter, r *http.Request) {
	logger := newRequestLogger(srv.logger, r, srv.rand)
	name := chi.URLParam(r, "name")
	namespace := chi.URLParam(r, "namespace")
	if name == "" {
		writeErrorResponse(logger, w, r, http.StatusBadRequest, "the following fields are missing or empty: name")
		return
	}
	if !srv.validateGetReportReq(logger, []string{"format"}, w, r) {
		return
	}
	srv.getReport(logger, name, namespace, r.Form["format"][0], true, true, w, r)
}

func (srv *server) getReportV2TableHandler(w http.ResponseWriter, r *http.Request) {
	logger := newRequestLogger(srv.logger, r, srv.rand)
	name := chi.URLParam(r, "name")
	namespace := chi.URLParam(r, "namespace")
	if name == "" {
		writeErrorResponse(logger, w, r, http.StatusBadRequest, "the following fields are missing or empty: name")
		return
	}
	if !srv.validateGetReportReq(logger, []string{"format"}, w, r) {
		return
	}
	srv.getReport(logger, name, namespace, r.Form["format"][0], true, false, w, r)
}

func checkForFields(fields []string, vals url.Values) error {
	var missingFields []string
	for _, f := range fields {
		if len(vals[f]) == 0 || vals[f][0] == "" {
			missingFields = append(missingFields, f)
		}
	}
	if len(missingFields) != 0 {
		return fmt.Errorf("the following fields are missing or empty: %s", strings.Join(missingFields, ","))
	}
	return nil
}

func (srv *server) getReport(logger log.FieldLogger, name, namespace, format string, useNewFormat bool, full bool, w http.ResponseWriter, r *http.Request) {
	// Get the report to make sure it hasn't failed
	report, err := srv.reportLister.Reports(namespace).Get(name)
	if err != nil {
		code := http.StatusInternalServerError
		if k8serrors.IsNotFound(err) {
			code = http.StatusNotFound
		}
		logger.WithError(err).Errorf("error getting report: %v", err)
		writeErrorResponse(logger, w, r, code, "error getting report: %v", err)
		return
	}

	if r.FormValue("ignore_failed") != "true" {
		if cond := cbutil.GetReportCondition(report.Status, api.ReportRunning); cond != nil && cond.Status == v1.ConditionFalse && cond.Reason == cbutil.GenerateReportFailedReason {
			logger.Errorf("report is is failed state, reason: %s, message: %s", cond.Reason, cond.Message)
			writeErrorResponse(logger, w, r, http.StatusInternalServerError, "report is is failed state, reason: %s, message: %s", cond.Reason, cond.Message)
			return
		}
	}

	reportQuery, err := srv.reportGenerationQuerieLister.ReportGenerationQueries(report.Namespace).Get(report.Spec.GenerationQueryName)
	if err != nil {
		logger.WithError(err).Errorf("error getting report: %v", err)
		writeErrorResponse(logger, w, r, http.StatusInternalServerError, "error getting report: %v", err)
		return
	}

	// Get the presto table to get actual columns in table
	prestoTable, err := srv.prestoTableLister.PrestoTables(report.Namespace).Get(reportingutil.PrestoTableResourceNameFromKind("report", report.Namespace, report.Name))
	if err != nil {
		if k8serrors.IsNotFound(err) {
			writeErrorResponse(logger, w, r, http.StatusAccepted, "Report is not processed yet")
			return
		}
		logger.WithError(err).Errorf("error getting presto table: %v", err)
		writeErrorResponse(logger, w, r, http.StatusInternalServerError, "error getting presto table: %v", err)
		return
	}

	tableColumns := prestoTable.Status.Parameters.Columns
	queryPrestoColumns, err := reportingutil.GeneratePrestoColumns(reportQuery)
	if err != nil {
		logger.WithError(err).Errorf("error converting ReportGenerationQuery columns to presto columns: %v", err)
		writeErrorResponse(logger, w, r, http.StatusInternalServerError, "error converting columns: %v", err)
		return
	}

	prestoColumns, err := reportingutil.HiveColumnsToPrestoColumns(tableColumns)
	if err != nil {
		logger.WithError(err).Errorf("error converting PrestoTable hive columns to presto columns: %v", err)
		writeErrorResponse(logger, w, r, http.StatusInternalServerError, "error converting columns: %v", err)
		return
	}

	if !reflect.DeepEqual(queryPrestoColumns, prestoColumns) {
		logger.Warnf("report columns and table columns don't match, ReportGenerationQuery was likely updated after the report ran")
		logger.Debugf("mismatched columns, PrestoTable columns: %v, ReportGenerationQuery columns: %v", prestoColumns, queryPrestoColumns)
	}

	tableName := reportingutil.ReportTableName(namespace, name)
	results, err := srv.reportResultsGetter.GetReportResults(tableName, prestoColumns)
	if err != nil {
		logger.WithError(err).Errorf("failed to perform presto query")
		writeErrorResponse(logger, w, r, http.StatusInternalServerError, "failed to perform presto query (see operator logs for more details): %v", err)
		return
	}

	if len(results) > 0 && len(prestoTable.Status.Parameters.Columns) != len(results[0]) {
		logger.Errorf("report results schema doesn't match expected schema, got %d columns, expected %d", len(results[0]), len(prestoTable.Status.Parameters.Columns))
		writeErrorResponse(logger, w, r, http.StatusInternalServerError, "report results schema doesn't match expected schema")
		return
	}

	if useNewFormat {
		writeResultsResponseV2(logger, full, format, reportQuery.Name, reportQuery.Spec.Columns, results, w, r)
	} else {
		writeResultsResponseV1(logger, format, reportQuery.Name, reportQuery.Spec.Columns, results, w, r)
	}
}

func writeResultsResponseAsCSV(logger log.FieldLogger, name string, columns []api.ReportGenerationQueryColumn, results []presto.Row, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment;filename=%s.csv", name))
	err := writeResultsAsCSV(columns, results, w, ',')
	if err != nil {
		writeErrorResponse(logger, w, r, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

func writeResultsAsCSV(columns []api.ReportGenerationQueryColumn, results []presto.Row, w io.Writer, delimiter rune) error {
	csvWriter := csv.NewWriter(w)
	csvWriter.Comma = delimiter

	// Write headers
	var keys []string
	if len(results) >= 1 {
		for _, column := range columns {
			keys = append(keys, column.Name)
		}
		err := csvWriter.Write(keys)
		if err != nil {
			return err
		}
	}

	// Write the rest
	for _, row := range results {
		vals := make([]string, len(keys))
		for i, key := range keys {
			val, ok := row[key]
			if !ok {
				return fmt.Errorf("report results schema doesn't match expected schema, unexpected key: %q", key)
			}
			switch v := val.(type) {
			case string:
				vals[i] = v
			case []byte:
				vals[i] = string(v)
			case uint, uint8, uint16, uint32, uint64, int, int8, int16, int32, int64:
				vals[i] = fmt.Sprintf("%d", v)
			case float32, float64, complex64, complex128:
				vals[i] = fmt.Sprintf("%f", v)
			case bool:
				vals[i] = fmt.Sprintf("%t", v)
			case time.Time:
				vals[i] = v.String()
			case nil:
				vals[i] = ""
			default:
				return fmt.Errorf("error marshalling csv: unknown type %t for value %v", val, val)
			}
		}
		err := csvWriter.Write(vals)
		if err != nil {
			return err
		}
	}

	csvWriter.Flush()
	return csvWriter.Error()
}

func writeResultsResponseAsTabular(logger log.FieldLogger, name string, columns []api.ReportGenerationQueryColumn, results []presto.Row, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/tab-separated-values")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment;filename=%s.tsv", name))
	var padding int = 2
	paddingStr := r.FormValue("padding")
	if paddingStr != "" {
		var err error
		padding, err = strconv.Atoi(paddingStr)
		if err != nil {
			writeErrorResponse(logger, w, r, http.StatusInternalServerError, "invalid padding value %s, err: %s", paddingStr, err)
			return
		}
	}
	tabWriter := tabwriter.NewWriter(w, 0, 8, padding, '\t', 0)
	err := writeResultsAsCSV(columns, results, tabWriter, '\t')
	if err != nil {
		writeErrorResponse(logger, w, r, http.StatusInternalServerError, err.Error())
		return
	}
	err = tabWriter.Flush()
	if err != nil {
		writeErrorResponse(logger, w, r, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

func writeResultsResponseAsJSON(logger log.FieldLogger, name string, columns []api.ReportGenerationQueryColumn, results []presto.Row, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment;filename=%s.json", name))
	newResults := make([]*orderedmap.OrderedMap, len(results))
	for i, item := range results {
		var err error
		newResults[i], err = orderedmap.NewFromMap(item)
		if err != nil {
			writeErrorResponse(logger, w, r, http.StatusInternalServerError, "error converting results: %v", err)
			return
		}
	}
	writeResponseAsJSON(logger, w, http.StatusOK, newResults)
}

func writeResultsResponse(logger log.FieldLogger, format, name string, columns []api.ReportGenerationQueryColumn, results []presto.Row, w http.ResponseWriter, r *http.Request) {
	switch format {
	case "json":
		writeResultsResponseAsJSON(logger, name, columns, results, w, r)
	case "csv":
		writeResultsResponseAsCSV(logger, name, columns, results, w, r)
	case "tab", "tabular":
		writeResultsResponseAsTabular(logger, name, columns, results, w, r)
	}
}

type GetReportResults struct {
	Results []ReportResultEntry `json:"results"`
}

type ReportResultEntry struct {
	Values []ReportResultValues `json:"values"`
}

type ReportResultValues struct {
	Name        string      `json:"name"`
	Value       interface{} `json:"value"`
	TableHidden bool        `json:"tableHidden"`
	Unit        string      `json:"unit,omitempty"`
}

// convertsToGetReportResults converts Rows returned from `presto.ExecuteSelect` into a GetReportResults
func convertsToGetReportResults(input []presto.Row, columns []api.ReportGenerationQueryColumn) GetReportResults {
	results := GetReportResults{}
	columnsMap := make(map[string]api.ReportGenerationQueryColumn)
	for _, column := range columns {
		columnsMap[column.Name] = column
	}
	for _, row := range input {
		var valSlice ReportResultEntry
		for columnName, columnValue := range row {
			resultsValue := ReportResultValues{
				Name:        columnName,
				Value:       columnValue,
				TableHidden: columnsMap[columnName].TableHidden,
				Unit:        columnsMap[columnName].Unit,
			}
			valSlice.Values = append(valSlice.Values, resultsValue)
		}
		results.Results = append(results.Results, valSlice)
	}
	return results
}

func writeResultsResponseV1(logger log.FieldLogger, format string, name string, columns []api.ReportGenerationQueryColumn, results []presto.Row, w http.ResponseWriter, r *http.Request) {
	columnsMap := make(map[string]api.ReportGenerationQueryColumn)
	var filteredColumns []api.ReportGenerationQueryColumn

	// remove tableHidden columns and their values if the format is tabular or CSV

	// filter columns
	for _, column := range columns {
		columnsMap[column.Name] = column
		showColumn := !columnsMap[column.Name].TableHidden
		if showColumn {
			filteredColumns = append(filteredColumns, column)
		}
	}

	// filter rows
	for _, row := range results {
		for _, column := range columnsMap {
			if columnsMap[column.Name].TableHidden {
				delete(row, columnsMap[column.Name].Name)
			}
		}
	}

	writeResultsResponse(logger, format, name, filteredColumns, results, w, r)
}

func writeResultsResponseV2(logger log.FieldLogger, full bool, format string, name string, columns []api.ReportGenerationQueryColumn, results []presto.Row, w http.ResponseWriter, r *http.Request) {
	format = strings.ToLower(format)
	isTableFormat := format == "csv" || format == "tab" || format == "tabular"
	columnsMap := make(map[string]api.ReportGenerationQueryColumn)
	var filteredColumns []api.ReportGenerationQueryColumn

	// Remove columns and their values from `results` if full is false and the
	// column's TableHidden is true or if TableHidden is true and we're
	// outputting tabular or CSV

	// filter the columns
	for _, column := range columns {
		columnsMap[column.Name] = column
		tableHidden := columnsMap[column.Name].TableHidden
		// skip using columns if tableHidden is true and we're outputing to
		// csv/tabular
		if tableHidden && (isTableFormat || !full) {
			continue
		}
		filteredColumns = append(filteredColumns, column)
	}

	// filter the rows
	for _, row := range results {
		for _, column := range columnsMap {
			tableHidden := columnsMap[column.Name].TableHidden
			if tableHidden && (isTableFormat || !full) {
				delete(row, columnsMap[column.Name].Name)
			}
		}
	}

	if format == "json" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment;filename=%s.json", name))
		writeResponseAsJSON(logger, w, http.StatusOK, convertsToGetReportResults(results, filteredColumns))
		return
	}

	writeResultsResponse(logger, format, name, filteredColumns, results, w, r)
}

func (srv *server) runReport(logger log.FieldLogger, query, start, end string, w http.ResponseWriter) {
	w.WriteHeader(http.StatusInternalServerError)
	w.Write([]byte("method not yet implemented"))
}

type CollectPromsumDataRequest struct {
	StartTime time.Time `json:"startTime"`
	EndTime   time.Time `json:"endTime"`
}

type CollectPromsumDataResponse struct {
	Results []*prometheusImportResults `json:"results"`
}

func (srv *server) collectPromsumDataHandler(w http.ResponseWriter, r *http.Request) {
	logger := newRequestLogger(srv.logger, r, srv.rand)

	namespace := chi.URLParam(r, "namespace")
	dsName := chi.URLParam(r, "datasource")

	decoder := json.NewDecoder(r.Body)
	var req CollectPromsumDataRequest
	err := decoder.Decode(&req)
	if err != nil {
		writeErrorResponse(logger, w, r, http.StatusInternalServerError, "unable to decode response as JSON: %v", err)
		return
	}

	start := req.StartTime.UTC()
	end := req.EndTime.UTC()

	logger.Debugf("collecting promsum data for ReportDataSources in namespace %s between %s and %s", namespace, start.Format(time.RFC3339), end.Format(time.RFC3339))

	results, err := srv.collectorFunc(context.Background(), namespace, dsName, start, end)
	if err != nil {
		writeErrorResponse(logger, w, r, http.StatusInternalServerError, "unable to collect prometheus data: %v", err)
		return
	}

	writeResponseAsJSON(logger, w, http.StatusOK, CollectPromsumDataResponse{
		Results: results,
	})
}

type StorePromsumDataRequest []*prestostore.PrometheusMetric

func (srv *server) storePromsumDataHandler(w http.ResponseWriter, r *http.Request) {
	logger := newRequestLogger(srv.logger, r, srv.rand)

	name := chi.URLParam(r, "datasourceName")
	namespace := chi.URLParam(r, "namespace")

	decoder := json.NewDecoder(r.Body)

	// read opening bracket
	_, err := decoder.Token()
	if err != nil {
		writeErrorResponse(logger, w, r, http.StatusInternalServerError, "unable to decode response as JSON: %v", err)
		return
	}

	var metrics []*prestostore.PrometheusMetric
	// while the array contains values
	for decoder.More() {
		var m prestostore.PrometheusMetric
		err = decoder.Decode(&m)
		if err != nil {
			writeErrorResponse(logger, w, r, http.StatusInternalServerError, "unable to decode response as JSON: %v", err)
			return
		}
		metrics = append(metrics, &m)
	}

	// read closing bracket
	_, err = decoder.Token()
	if err != nil {
		writeErrorResponse(logger, w, r, http.StatusInternalServerError, "unable to decode response as JSON: %v", err)
		return
	}

	err = srv.prometheusMetricsRepo.StorePrometheusMetrics(context.Background(), reportingutil.DataSourceTableName(namespace, name), metrics)
	if err != nil {
		writeErrorResponse(logger, w, r, http.StatusInternalServerError, "unable to store promsum metrics: %v", err)
		return
	}

	writeResponseAsJSON(logger, w, http.StatusOK, struct{}{})
}

func (srv *server) fetchPromsumDataHandler(w http.ResponseWriter, r *http.Request) {
	logger := newRequestLogger(srv.logger, r, srv.rand)

	name := chi.URLParam(r, "datasourceName")
	namespace := chi.URLParam(r, "namespace")
	err := r.ParseForm()
	if err != nil {
		writeErrorResponse(logger, w, r, http.StatusInternalServerError, "unable to decode body: %v", err)
		return
	}

	datasourceTable := reportingutil.DataSourceTableName(namespace, name)
	start := r.Form.Get("start")
	end := r.Form.Get("end")
	var startTime, endTime time.Time
	if start != "" {
		startTime, err = time.Parse(time.RFC3339, start)
		if err != nil {
			writeErrorResponse(logger, w, r, http.StatusInternalServerError, "invalid start time parameter: %v", err)
			return
		}
	}
	if end != "" {
		endTime, err = time.Parse(time.RFC3339, end)
		if err != nil {
			writeErrorResponse(logger, w, r, http.StatusInternalServerError, "invalid end time parameter: %v", err)
			return
		}
	}
	results, err := srv.prometheusMetricsRepo.GetPrometheusMetrics(datasourceTable, startTime, endTime)
	if err != nil {
		writeErrorResponse(logger, w, r, http.StatusInternalServerError, "error querying for datasource: %v", err)
		return
	}

	writeResponseAsJSON(logger, w, http.StatusOK, results)
}
