package pdf

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/form"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/sammcj/mcp-devtools/internal/registry"
	"github.com/sammcj/mcp-devtools/internal/security"
	"github.com/sammcj/mcp-devtools/internal/tools"
	"github.com/sirupsen/logrus"
)

// PDFEditorTool implements PDF form editing with pdfcpu
type PDFEditorTool struct{}

// init registers the PDF editor tool
func init() {
	registry.Register(&PDFEditorTool{})
}

// Definition returns the tool's definition for MCP registration
func (t *PDFEditorTool) Definition() mcp.Tool {
	tool := mcp.NewTool(
		"pdf_editor",
		mcp.WithDescription(`Edit PDF form fields . Supports listing form fields, exporting form data to JSON, and filling form fields with new values. This tool allows you to programmatically interact with PDF forms.`),
		mcp.WithString("file_path",
			mcp.Required(),
			mcp.Description("Absolute file path to the PDF document to edit"),
		),
		mcp.WithString("operation",
			mcp.Required(),
			mcp.Description("Operation to perform: 'list' (list all form fields), 'export' (export form data to JSON), or 'fill' (fill form fields with values)"),
		),
		mcp.WithString("output_file",
			mcp.Description("Output file path for 'export' or 'fill' operations (defaults to same directory as input PDF)"),
		),
		mcp.WithString("form_data",
			mcp.Description("JSON string containing form field values for 'fill' operation (e.g., '{\"field_name\": \"value\"}')"),
		),

		// Annotations for operation hints
		mcp.WithReadOnlyHintAnnotation(false),    // Can modify PDFs when filling
		mcp.WithDestructiveHintAnnotation(false), // Creates new files, doesn't modify originals
		mcp.WithIdempotentHintAnnotation(true),   // Same input produces same output
		mcp.WithOpenWorldHintAnnotation(false),   // Works with local files only
	)
	return tool
}

// Execute processes the PDF form editing request
func (t *PDFEditorTool) Execute(ctx context.Context, logger *logrus.Logger, cache *sync.Map, args map[string]any) (*mcp.CallToolResult, error) {
	logger.Debug("Executing PDF editor tool")

	// Parse and validate parameters
	request, err := t.ParseRequest(args)
	if err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	logger.WithFields(logrus.Fields{
		"file_path":   request.FilePath,
		"operation":   request.Operation,
		"output_file": request.OutputFile,
	}).Debug("PDF editor parameters")

	// Security check for input file access
	if err := security.CheckFileAccess(request.FilePath); err != nil {
		return nil, err
	}

	// Validate input file exists and check file size
	fileInfo, err := os.Stat(request.FilePath)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("PDF file does not exist: %s", request.FilePath)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to stat PDF file: %w", err)
	}

	// Apply file size limits (reuse from PDFTool)
	pdfTool := &PDFTool{}
	if err := pdfTool.ValidateFileSize(fileInfo.Size()); err != nil {
		return nil, fmt.Errorf("file size validation failed: %w", err)
	}

	// Create configuration with memory limits
	conf := model.NewDefaultConfiguration()
	pdfTool.applyMemoryLimits(conf)

	// Execute the requested operation
	var result any
	switch request.Operation {
	case "list":
		result, err = t.listFormFields(logger, request, conf)
	case "export":
		result, err = t.exportFormData(logger, request, conf)
	case "fill":
		result, err = t.fillFormFields(logger, request, conf)
	default:
		return nil, fmt.Errorf("invalid operation: %s (must be 'list', 'export', or 'fill')", request.Operation)
	}

	if err != nil {
		return t.newToolResultJSON(map[string]any{
			"error":     err.Error(),
			"file_path": request.FilePath,
			"operation": request.Operation,
		})
	}

	logger.WithFields(logrus.Fields{
		"file_path": request.FilePath,
		"operation": request.Operation,
	}).Debug("PDF editor operation completed successfully")

	return t.newToolResultJSON(result)
}

// ParseRequest parses and validates the tool arguments
func (t *PDFEditorTool) ParseRequest(args map[string]any) (*pdfEditorRequest, error) {
	// Parse file_path (required)
	filePath, ok := args["file_path"].(string)
	if !ok || filePath == "" {
		return nil, fmt.Errorf("missing or invalid required parameter: file_path")
	}

	// Validate file path is absolute
	if !filepath.IsAbs(filePath) {
		return nil, fmt.Errorf("file_path must be an absolute path")
	}

	// Validate file extension
	if !strings.HasSuffix(strings.ToLower(filePath), ".pdf") {
		return nil, fmt.Errorf("file_path must be a PDF file (.pdf extension)")
	}

	// Parse operation (required)
	operation, ok := args["operation"].(string)
	if !ok || operation == "" {
		return nil, fmt.Errorf("missing or invalid required parameter: operation")
	}

	// Validate operation
	operation = strings.ToLower(operation)
	if operation != "list" && operation != "export" && operation != "fill" {
		return nil, fmt.Errorf("operation must be 'list', 'export', or 'fill'")
	}

	request := &pdfEditorRequest{
		FilePath:  filePath,
		Operation: operation,
	}

	// Parse output_file (optional, but required for export and fill)
	if outputFile, ok := args["output_file"].(string); ok && outputFile != "" {
		if !filepath.IsAbs(outputFile) {
			return nil, fmt.Errorf("output_file must be an absolute path")
		}
		request.OutputFile = outputFile
	} else {
		// Generate default output file based on operation
		if operation == "export" {
			request.OutputFile = strings.TrimSuffix(filePath, ".pdf") + "_form_data.json"
		} else if operation == "fill" {
			request.OutputFile = strings.TrimSuffix(filePath, ".pdf") + "_filled.pdf"
		}
	}

	// Security check for output file access (if applicable)
	if request.OutputFile != "" {
		outputDir := filepath.Dir(request.OutputFile)
		if err := security.CheckFileAccess(outputDir); err != nil {
			return nil, fmt.Errorf("output file access denied: %w", err)
		}
	}

	// Parse form_data (required for fill operation)
	if operation == "fill" {
		formDataStr, ok := args["form_data"].(string)
		if !ok || formDataStr == "" {
			return nil, fmt.Errorf("form_data is required for 'fill' operation")
		}

		// Parse JSON form data
		var formData map[string]any
		if err := json.Unmarshal([]byte(formDataStr), &formData); err != nil {
			return nil, fmt.Errorf("invalid form_data JSON: %w", err)
		}

		request.FormData = formData
	}

	return request, nil
}

// listFormFields lists all form fields in the PDF
func (t *PDFEditorTool) listFormFields(logger *logrus.Logger, request *pdfEditorRequest, conf *model.Configuration) (*pdfEditorResponse, error) {
	logger.Debug("Listing form fields from PDF")

	// Open the PDF file
	f, err := os.Open(request.FilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open PDF file: %w", err)
	}
	defer f.Close()

	// Get form fields
	fields, err := api.FormFields(f, conf)
	if err != nil {
		// Provide more helpful error messages for common issues
		errMsg := err.Error()
		if strings.Contains(errMsg, "FontFamily") {
			return nil, fmt.Errorf("PDF form compatibility issue: This PDF contains unsupported font entries. The PDF may not be a standard AcroForm or may require a different PDF library. Original error: %w", err)
		}
		if strings.Contains(errMsg, "no form") || strings.Contains(errMsg, "AcroForm") {
			return nil, fmt.Errorf("no interactive form found: This PDF does not contain fillable form fields (AcroForm). It may be a static PDF or use a different form technology. Original error: %w", err)
		}
		return nil, fmt.Errorf("failed to get form fields: %w. This may indicate the PDF is not a standard fillable form or has compatibility issues with pdfcpu", err)
	}

	if len(fields) == 0 {
		return &pdfEditorResponse{
			FilePath:  request.FilePath,
			Operation: "list",
			Message:   "No form fields found in PDF",
			Fields:    []formFieldInfo{},
		}, nil
	}

	// Convert fields to response format
	fieldInfos := make([]formFieldInfo, 0, len(fields))
	for _, field := range fields {
		fieldInfo := formFieldInfo{
			Name:     field.Name,
			Type:     field.Typ.String(),
			Value:    field.V,
			Default:  field.Dv,
			Options:  field.Opts, // This is a string, not []string
			ReadOnly: field.Locked,
		}
		fieldInfos = append(fieldInfos, fieldInfo)
	}

	logger.WithField("field_count", len(fieldInfos)).Debug("Form fields listed successfully")

	return &pdfEditorResponse{
		FilePath:   request.FilePath,
		Operation:  "list",
		FieldCount: len(fieldInfos),
		Fields:     fieldInfos,
	}, nil
}

// exportFormData exports form data to JSON
func (t *PDFEditorTool) exportFormData(logger *logrus.Logger, request *pdfEditorRequest, conf *model.Configuration) (*pdfEditorResponse, error) {
	logger.WithField("output_file", request.OutputFile).Debug("Exporting form data to JSON")

	// Export form data using pdfcpu API
	err := api.ExportFormFile(request.FilePath, request.OutputFile, conf)
	if err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "FontFamily") {
			return nil, fmt.Errorf("PDF form compatibility issue: This PDF contains unsupported font entries. The PDF may not be a standard AcroForm or may require a different PDF library. Original error: %w", err)
		}
		if strings.Contains(errMsg, "no form") || strings.Contains(errMsg, "AcroForm") {
			return nil, fmt.Errorf("no interactive form found: This PDF does not contain fillable form fields (AcroForm). Original error: %w", err)
		}
		return nil, fmt.Errorf("failed to export form data: %w. This may indicate the PDF is not a standard fillable form or has compatibility issues", err)
	}

	// Read the exported JSON to get field count
	jsonData, err := os.ReadFile(request.OutputFile)
	if err != nil {
		logger.WithError(err).Warn("Failed to read exported JSON file")
	}

	var formGroup form.FormGroup
	fieldCount := 0
	if err == nil {
		if err := json.Unmarshal(jsonData, &formGroup); err == nil {
			// Count all fields across all forms
			for _, f := range formGroup.Forms {
				fieldCount += len(f.TextFields) + len(f.DateFields) + len(f.CheckBoxes) +
					len(f.RadioButtonGroups) + len(f.ComboBoxes) + len(f.ListBoxes)
			}
		}
	}

	logger.WithFields(logrus.Fields{
		"output_file": request.OutputFile,
		"field_count": fieldCount,
	}).Debug("Form data exported successfully")

	return &pdfEditorResponse{
		FilePath:   request.FilePath,
		Operation:  "export",
		OutputFile: request.OutputFile,
		FieldCount: fieldCount,
		Message:    fmt.Sprintf("Form data exported to %s", request.OutputFile),
	}, nil
}

// fillFormFields fills form fields with provided values
func (t *PDFEditorTool) fillFormFields(logger *logrus.Logger, request *pdfEditorRequest, conf *model.Configuration) (*pdfEditorResponse, error) {
	logger.WithField("output_file", request.OutputFile).Debug("Filling form fields")

	// First, export the current form structure to a temp file
	tempExportFile, err := os.CreateTemp("", "pdfcpu_export_*.json")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp export file: %w", err)
	}
	tempExportFile.Close()
	defer os.Remove(tempExportFile.Name())

	// Export current form structure
	err = api.ExportFormFile(request.FilePath, tempExportFile.Name(), conf)
	if err != nil {
		return nil, fmt.Errorf("failed to export form structure: %w", err)
	}

	// Read the exported form data
	exportedData, err := os.ReadFile(tempExportFile.Name())
	if err != nil {
		return nil, fmt.Errorf("failed to read exported form data: %w", err)
	}

	var formGroup form.FormGroup
	if err := json.Unmarshal(exportedData, &formGroup); err != nil {
		return nil, fmt.Errorf("failed to parse form data: %w", err)
	}

	if len(formGroup.Forms) == 0 {
		return nil, fmt.Errorf("no forms found in PDF")
	}

	// Update form fields with provided values
	updatedCount := 0
	totalFields := 0

	// Update text fields
	for i := range formGroup.Forms {
		formData := &formGroup.Forms[i]

		// Update text fields
		for j := range formData.TextFields {
			totalFields++
			field := formData.TextFields[j]
			if value, ok := request.FormData[field.Name]; ok {
				field.Value = fmt.Sprintf("%v", value)
				updatedCount++
				logger.WithFields(logrus.Fields{
					"field_name":  field.Name,
					"field_value": field.Value,
				}).Debug("Updated text field")
			}
		}

		// Update date fields
		for j := range formData.DateFields {
			totalFields++
			field := formData.DateFields[j]
			if value, ok := request.FormData[field.Name]; ok {
				field.Value = fmt.Sprintf("%v", value)
				updatedCount++
				logger.WithFields(logrus.Fields{
					"field_name":  field.Name,
					"field_value": field.Value,
				}).Debug("Updated date field")
			}
		}

		// Update checkboxes
		for j := range formData.CheckBoxes {
			totalFields++
			field := formData.CheckBoxes[j]
			if value, ok := request.FormData[field.Name]; ok {
				// Convert to boolean
				var boolValue bool
				switch v := value.(type) {
				case bool:
					boolValue = v
				case string:
					boolValue = (v == "Yes" || v == "yes" || v == "true" || v == "True" || v == "1" || v == "On" || v == "on")
				case float64:
					boolValue = v != 0
				default:
					boolValue = false
				}
				field.Value = boolValue
				updatedCount++
				logger.WithFields(logrus.Fields{
					"field_name":  field.Name,
					"field_value": field.Value,
				}).Debug("Updated checkbox")
			}
		}

		// Update combo boxes
		for j := range formData.ComboBoxes {
			totalFields++
			field := formData.ComboBoxes[j]
			if value, ok := request.FormData[field.Name]; ok {
				field.Value = fmt.Sprintf("%v", value)
				updatedCount++
				logger.WithFields(logrus.Fields{
					"field_name":  field.Name,
					"field_value": field.Value,
				}).Debug("Updated combo box")
			}
		}

		// Update list boxes
		for j := range formData.ListBoxes {
			totalFields++
			field := formData.ListBoxes[j]
			if value, ok := request.FormData[field.Name]; ok {
				// ListBox uses Values ([]string) not Value
				valueStr := fmt.Sprintf("%v", value)
				field.Values = []string{valueStr}
				updatedCount++
				logger.WithFields(logrus.Fields{
					"field_name":  field.Name,
					"field_value": field.Values,
				}).Debug("Updated list box")
			}
		}
	}

	if updatedCount == 0 {
		return nil, fmt.Errorf("no matching form fields found for provided data")
	}

	// Create a temporary JSON file with updated form data
	tempJSONFile, err := os.CreateTemp("", "pdfcpu_form_*.json")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp JSON file: %w", err)
	}
	defer os.Remove(tempJSONFile.Name())
	defer tempJSONFile.Close()

	// Write updated form data to temp JSON
	encoder := json.NewEncoder(tempJSONFile)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(formGroup); err != nil {
		return nil, fmt.Errorf("failed to encode form data: %w", err)
	}
	tempJSONFile.Close()

	// Fill the form using the temp JSON file
	err = api.FillFormFile(request.FilePath, tempJSONFile.Name(), request.OutputFile, conf)
	if err != nil {
		return nil, fmt.Errorf("failed to fill form: %w", err)
	}

	logger.WithFields(logrus.Fields{
		"output_file":   request.OutputFile,
		"updated_count": updatedCount,
	}).Debug("Form fields filled successfully")

	return &pdfEditorResponse{
		FilePath:     request.FilePath,
		Operation:    "fill",
		OutputFile:   request.OutputFile,
		FieldCount:   totalFields,
		UpdatedCount: updatedCount,
		Message:      fmt.Sprintf("Filled %d form fields and saved to %s", updatedCount, request.OutputFile),
	}, nil
}

// newToolResultJSON creates a new tool result with JSON content
func (t *PDFEditorTool) newToolResultJSON(data any) (*mcp.CallToolResult, error) {
	jsonBytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal JSON: %w", err)
	}

	return mcp.NewToolResultText(string(jsonBytes)), nil
}

// ProvideExtendedInfo provides detailed usage information for the PDF editor tool
func (t *PDFEditorTool) ProvideExtendedInfo() *tools.ExtendedHelp {
	return &tools.ExtendedHelp{
		Examples: []tools.ToolExample{
			{
				Description: "List all form fields in a PDF",
				Arguments: map[string]any{
					"file_path": "/Users/username/documents/form.pdf",
					"operation": "list",
				},
				ExpectedResult: "Returns a list of all form fields with their names, types, current values, and options",
			},
			{
				Description: "Export form data to JSON",
				Arguments: map[string]any{
					"file_path":   "/Users/username/documents/form.pdf",
					"operation":   "export",
					"output_file": "/Users/username/documents/form_data.json",
				},
				ExpectedResult: "Exports all form field data to a JSON file that can be edited and used for filling",
			},
			{
				Description: "Fill form fields with values",
				Arguments: map[string]any{
					"file_path":   "/Users/username/documents/form.pdf",
					"operation":   "fill",
					"output_file": "/Users/username/documents/form_filled.pdf",
					"form_data":   `{"name": "John Doe", "email": "john@example.com", "agree": true}`,
				},
				ExpectedResult: "Creates a new PDF with the specified form fields filled in",
			},
			{
				Description: "Fill form with default output location",
				Arguments: map[string]any{
					"file_path": "/Users/username/documents/application.pdf",
					"operation": "fill",
					"form_data": `{"applicant_name": "Jane Smith", "date": "2024-01-15", "signature": "Jane Smith"}`,
				},
				ExpectedResult: "Creates application_filled.pdf in the same directory with filled form fields",
			},
		},
		CommonPatterns: []string{
			"Start with 'list' operation to discover available form fields and their types",
			"Use 'export' to get a template JSON file, then modify it and use with 'fill' operation",
			"For boolean fields (checkboxes), use true/false or 'Yes'/'No' strings",
			"Field names are case-sensitive and must match exactly",
			"Use 'fill' operation multiple times with different data to create multiple filled forms",
		},
		Troubleshooting: []tools.TroubleshootingTip{
			{
				Problem:  "No form fields found in PDF",
				Solution: "The PDF may not contain interactive form fields (AcroForms). Check if the PDF has fillable fields using a PDF viewer. Scanned PDFs or PDFs with static text won't have form fields.",
			},
			{
				Problem:  "Form field not updated after fill operation",
				Solution: "Ensure the field name in form_data exactly matches the field name from 'list' operation (case-sensitive). Some fields may be read-only or locked.",
			},
			{
				Problem:  "Invalid form_data JSON error",
				Solution: "Ensure form_data is valid JSON. Use double quotes for keys and string values. Escape special characters properly. Test JSON validity with a JSON validator.",
			},
			{
				Problem:  "Output file permission errors",
				Solution: "Ensure the output directory exists and has write permissions. The tool creates new files but needs write access to the parent directory.",
			},
			{
				Problem:  "Checkbox or radio button not working",
				Solution: "For checkboxes/radio buttons, use boolean values (true/false) or string values like 'Yes'/'No', 'On'/'Off'. Check the field's options using 'list' operation to see valid values.",
			},
		},
		ParameterDetails: map[string]string{
			"file_path":   "Absolute path to PDF file with form fields (required). Must be a valid PDF with AcroForm fields. File size limits apply for security.",
			"operation":   "Operation to perform (required): 'list' to view fields, 'export' to save form data as JSON, 'fill' to populate fields with values.",
			"output_file": "Output file path (optional for list, required for export/fill). For export: JSON file path. For fill: new PDF file path. Defaults to input filename with suffix.",
			"form_data":   "JSON string with field values for fill operation (required for fill). Format: {\"field_name\": \"value\"}. Supports strings, numbers, and booleans.",
		},
		WhenToUse:    "Use for automating PDF form filling, batch processing forms with different data, extracting form data for analysis, or creating pre-filled forms from templates.",
		WhenNotToUse: "Don't use for PDFs without interactive form fields, password-protected PDFs, or when you need to add new form fields (this tool only fills existing fields).",
	}
}

// pdfEditorRequest represents a request to edit a PDF
type pdfEditorRequest struct {
	FilePath   string         `json:"file_path"`
	Operation  string         `json:"operation"` // "list", "export", or "fill"
	OutputFile string         `json:"output_file"`
	FormData   map[string]any `json:"form_data"`
}

// pdfEditorResponse represents the response from a PDF editor operation
type pdfEditorResponse struct {
	FilePath     string          `json:"file_path"`
	Operation    string          `json:"operation"`
	OutputFile   string          `json:"output_file,omitempty"`
	FieldCount   int             `json:"field_count,omitempty"`
	UpdatedCount int             `json:"updated_count,omitempty"`
	Fields       []formFieldInfo `json:"fields,omitempty"`
	Message      string          `json:"message,omitempty"`
}

// formFieldInfo represents information about a PDF form field
type formFieldInfo struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Value    string `json:"value,omitempty"`
	Default  string `json:"default,omitempty"`
	Options  string `json:"options,omitempty"` // Changed from []string to string
	ReadOnly bool   `json:"read_only"`
}
