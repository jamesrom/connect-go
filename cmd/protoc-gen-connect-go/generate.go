// Copyright 2021-2022 Buf Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/types/descriptorpb"
)

const (
	contextPackage = protogen.GoImportPath("context")
	errorsPackage  = protogen.GoImportPath("errors")
	httpPackage    = protogen.GoImportPath("net/http")
	stringsPackage = protogen.GoImportPath("strings")

	connectPackage = protogen.GoImportPath("github.com/bufbuild/connect")
	protoPackage   = protogen.GoImportPath("google.golang.org/protobuf/proto")

	commentWidth = 97 // leave room for "// "
)

var contextContext = contextPackage.Ident("Context")

func generate(gen *protogen.Plugin, file *protogen.File) *protogen.GeneratedFile {
	if len(file.Services) == 0 {
		return nil
	}
	filename := file.GeneratedFilenamePrefix + ".connect.go"
	var path protogen.GoImportPath
	g := gen.NewGeneratedFile(filename, path)
	preamble(gen, file, g)
	content(file, g)
	return g
}

func preamble(gen *protogen.Plugin, file *protogen.File, g *protogen.GeneratedFile) {
	g.P("// Code generated by protoc-gen-connect-go. DO NOT EDIT.")
	g.P("//")
	if file.Proto.GetOptions().GetDeprecated() {
		wrapComments(g, file.Desc.Path(), " is a deprecated file.")
	} else {
		g.P("// Source: ", file.Desc.Path())
	}
	g.P()
	g.P("package ", file.GoPackageName)
	g.P()
}

func content(file *protogen.File, g *protogen.GeneratedFile) {
	if len(file.Services) == 0 {
		return
	}
	handshake(g)
	for _, svc := range file.Services {
		service(file, g, svc)
	}
}

func handshake(g *protogen.GeneratedFile) {
	wrapComments(g, "This is a compile-time assertion to ensure that this generated file ",
		"and the connect package are compatible. If you get a compiler error that this constant ",
		"isn't defined, this code was generated with a version of connect newer than the one ",
		"compiled into your binary. You can fix the problem by either regenerating this code ",
		"with an older version of connect or updating the connect version compiled into your binary.")
	g.P("const _ = ", connectPackage.Ident("IsAtLeastVersion0_0_1"))
	g.P()
}

type names struct {
	Base string

	Client             string
	ClientConstructor  string
	ClientImpl         string
	ClientExposeMethod string

	Server              string
	ServerConstructor   string
	UnimplementedServer string
}

func newNames(service *protogen.Service) names {
	base := service.GoName
	return names{
		Base: base,

		Client:            fmt.Sprintf("%sClient", base),
		ClientConstructor: fmt.Sprintf("New%sClient", base),
		ClientImpl:        fmt.Sprintf("%sClient", unexport(base)),

		Server:              fmt.Sprintf("%sHandler", base),
		ServerConstructor:   fmt.Sprintf("New%sHandler", base),
		UnimplementedServer: fmt.Sprintf("Unimplemented%sHandler", base),
	}
}

func service(file *protogen.File, g *protogen.GeneratedFile, service *protogen.Service) {
	names := newNames(service)

	clientInterface(g, service, names)
	clientImplementation(g, service, names)

	serverInterface(g, service, names)
	serverConstructor(g, service, names)
	unimplementedServerImplementation(g, service, names)
}

func clientInterface(g *protogen.GeneratedFile, service *protogen.Service, names names) {
	wrapComments(g, names.Client, " is a client for the ", service.Desc.FullName(), " service.")
	if service.Desc.Options().(*descriptorpb.ServiceOptions).GetDeprecated() {
		g.P("//")
		deprecated(g)
	}
	g.Annotate(names.Client, service.Location)
	g.P("type ", names.Client, " interface {")
	for _, method := range service.Methods {
		g.Annotate(names.Client+"."+method.GoName, method.Location)
		leadingComments(
			g,
			method.Comments.Leading,
			method.Desc.Options().(*descriptorpb.MethodOptions).GetDeprecated(),
		)
		g.P(clientSignature(g, method, false /* named */))
	}
	g.P("}")
	g.P()
}

func clientSignature(g *protogen.GeneratedFile, method *protogen.Method, named bool) string {
	reqName := "req"
	ctxName := "ctx"
	if !named {
		reqName, ctxName = "", ""
	}
	if method.Desc.IsStreamingClient() && method.Desc.IsStreamingServer() {
		// bidi streaming
		return method.GoName + "(" + ctxName + " " + g.QualifiedGoIdent(contextContext) + ") " +
			"*" + g.QualifiedGoIdent(connectPackage.Ident("BidiStreamForClient")) +
			"[" + g.QualifiedGoIdent(method.Input.GoIdent) + ", " + g.QualifiedGoIdent(method.Output.GoIdent) + "]"
	}
	if method.Desc.IsStreamingClient() {
		// client streaming
		return method.GoName + "(" + ctxName + " " + g.QualifiedGoIdent(contextContext) + ") " +
			"*" + g.QualifiedGoIdent(connectPackage.Ident("ClientStreamForClient")) +
			"[" + g.QualifiedGoIdent(method.Input.GoIdent) + ", " + g.QualifiedGoIdent(method.Output.GoIdent) + "]"
	}
	if method.Desc.IsStreamingServer() {
		return method.GoName + "(" + ctxName + " " + g.QualifiedGoIdent(contextContext) +
			", " + reqName + " *" + g.QualifiedGoIdent(connectPackage.Ident("Envelope")) + "[" +
			g.QualifiedGoIdent(method.Input.GoIdent) + "]) " +
			"(*" + g.QualifiedGoIdent(connectPackage.Ident("ServerStreamForClient")) +
			"[" + g.QualifiedGoIdent(method.Output.GoIdent) + "]" +
			", error)"
	}
	// unary; symmetric so we can re-use server templating
	return method.GoName + serverSignatureParams(g, method, named)
}

func procedureName(method *protogen.Method) string {
	return fmt.Sprintf(
		"/%s.%s/%s",
		method.Parent.Desc.ParentFile().Package(),
		method.Parent.Desc.Name(),
		method.Desc.Name(),
	)
}

func reflectionName(service *protogen.Service) string {
	return fmt.Sprintf("%s.%s", service.Desc.ParentFile().Package(), service.Desc.Name())
}

func clientImplementation(g *protogen.GeneratedFile, service *protogen.Service, names names) {
	clientOption := connectPackage.Ident("ClientOption")

	// Client constructor.
	wrapComments(g, names.ClientConstructor, " constructs a client for the ", service.Desc.FullName(),
		" service. By default, it uses the binary protobuf Codec, ",
		"asks for gzipped responses, and sends uncompressed requests. ",
		"It doesn't have a default protocol; you must supply either the connect.WithGRPC() or ",
		"connect.WithGRPCWeb() options.")
	g.P("//")
	wrapComments(g, "The URL supplied here should be the base URL for the gRPC server ",
		"(e.g., https://api.acme.com or https://acme.com/grpc).")
	if service.Desc.Options().(*descriptorpb.ServiceOptions).GetDeprecated() {
		g.P("//")
		deprecated(g)
	}
	g.P("func ", names.ClientConstructor, " (doer ", connectPackage.Ident("Doer"),
		", baseURL string, opts ...", clientOption, ") (", names.Client, ", error) {")
	g.P("baseURL = ", stringsPackage.Ident("TrimRight"), `(baseURL, "/")`)
	for _, method := range service.Methods {
		g.P(unexport(method.GoName), "Client, err := ",
			connectPackage.Ident("NewClient"),
			"[", method.Input.GoIdent, ", ", method.Output.GoIdent, "]",
			"(",
		)
		g.P("doer,")
		g.P(`baseURL + "`, procedureName(method), `",`)
		g.P("opts...,")
		g.P(")")
		g.P("if err != nil {")
		g.P("return nil, err")
		g.P("}")
	}
	g.P("return &", names.ClientImpl, "{")
	for _, method := range service.Methods {
		g.P(unexport(method.GoName), ": ", unexport(method.GoName), "Client,")
	}
	g.P("}, nil")
	g.P("}")
	g.P()

	// Client struct.
	wrapComments(g, names.ClientImpl, " implements ", names.Client, ".")
	g.P("type ", names.ClientImpl, " struct {")
	for _, method := range service.Methods {
		g.P(unexport(method.GoName), " *", connectPackage.Ident("Client"),
			"[", method.Input.GoIdent, ", ", method.Output.GoIdent, "]")
	}
	g.P("}")
	g.P()
	for _, method := range service.Methods {
		clientMethod(g, service, method, names)
	}
}

func clientMethod(g *protogen.GeneratedFile, service *protogen.Service, method *protogen.Method, names names) {
	receiver := names.ClientImpl
	isStreamingClient := method.Desc.IsStreamingClient()
	isStreamingServer := method.Desc.IsStreamingServer()
	wrapComments(g, method.GoName, " calls ", method.Desc.FullName(), ".")
	if method.Desc.Options().(*descriptorpb.MethodOptions).GetDeprecated() {
		g.P("//")
		deprecated(g)
	}
	g.P("func (c *", receiver, ") ", clientSignature(g, method, true /* named */), " {")

	if isStreamingClient && !isStreamingServer {
		g.P("return c.", unexport(method.GoName), ".CallClientStream(ctx)")
	} else if !isStreamingClient && isStreamingServer {
		g.P("return c.", unexport(method.GoName), ".CallServerStream(ctx, req)")
	} else if isStreamingClient && isStreamingServer {
		g.P("return c.", unexport(method.GoName), ".CallBidiStream(ctx)")
	} else {
		g.P("return c.", unexport(method.GoName), ".CallUnary(ctx, req)")
	}
	g.P("}")
	g.P()
}

func serverInterface(g *protogen.GeneratedFile, service *protogen.Service, names names) {
	wrapComments(g, names.Server, " is an implementation of the ", service.Desc.FullName(), " service.")
	if service.Desc.Options().(*descriptorpb.ServiceOptions).GetDeprecated() {
		g.P("//")
		deprecated(g)
	}
	g.Annotate(names.Server, service.Location)
	g.P("type ", names.Server, " interface {")
	for _, method := range service.Methods {
		leadingComments(
			g,
			method.Comments.Leading,
			method.Desc.Options().(*descriptorpb.MethodOptions).GetDeprecated(),
		)
		g.Annotate(names.Server+"."+method.GoName, method.Location)
		g.P(serverSignature(g, method))
	}
	g.P("}")
	g.P()
}

func serverSignature(g *protogen.GeneratedFile, method *protogen.Method) string {
	return method.GoName + serverSignatureParams(g, method, false /* named */)
}

func serverSignatureParams(g *protogen.GeneratedFile, method *protogen.Method, named bool) string {
	ctxName := "ctx "
	reqName := "req "
	streamName := "stream "
	if !named {
		ctxName, reqName, streamName = "", "", ""
	}
	if method.Desc.IsStreamingClient() && method.Desc.IsStreamingServer() {
		// bidi streaming
		return "(" + ctxName + g.QualifiedGoIdent(contextContext) + ", " +
			streamName + "*" + g.QualifiedGoIdent(connectPackage.Ident("BidiStream")) +
			"[" + g.QualifiedGoIdent(method.Input.GoIdent) + ", " + g.QualifiedGoIdent(method.Output.GoIdent) + "]" +
			") error"
	}
	if method.Desc.IsStreamingClient() {
		// client streaming
		return "(" + ctxName + g.QualifiedGoIdent(contextContext) + ", " +
			streamName + "*" + g.QualifiedGoIdent(connectPackage.Ident("ClientStream")) +
			"[" + g.QualifiedGoIdent(method.Input.GoIdent) + ", " + g.QualifiedGoIdent(method.Output.GoIdent) + "]" +
			") error"
	}
	if method.Desc.IsStreamingServer() {
		// server streaming
		return "(" + ctxName + g.QualifiedGoIdent(contextContext) +
			", " + reqName + "*" + g.QualifiedGoIdent(connectPackage.Ident("Envelope")) + "[" +
			g.QualifiedGoIdent(method.Input.GoIdent) + "], " +
			streamName + "*" + g.QualifiedGoIdent(connectPackage.Ident("ServerStream")) +
			"[" + g.QualifiedGoIdent(method.Output.GoIdent) + "]" +
			") error"
	}
	// unary
	return "(" + ctxName + g.QualifiedGoIdent(contextContext) +
		", " + reqName + "*" + g.QualifiedGoIdent(connectPackage.Ident("Envelope")) + "[" +
		g.QualifiedGoIdent(method.Input.GoIdent) + "]) " +
		"(*" + g.QualifiedGoIdent(connectPackage.Ident("Envelope")) + "[" +
		g.QualifiedGoIdent(method.Output.GoIdent) + "], error)"
}

func serverConstructor(g *protogen.GeneratedFile, service *protogen.Service, names names) {
	wrapComments(g, names.ServerConstructor, " builds an HTTP handler from the service implementation.",
		" It returns the path on which to mount the handler and the handler itself.")
	g.P("//")
	wrapComments(g, "By default, handlers support the gRPC and gRPC-Web protocols with ",
		"the binary protobuf and JSON codecs.")
	if service.Desc.Options().(*descriptorpb.ServiceOptions).GetDeprecated() {
		g.P("//")
		deprecated(g)
	}
	handlerOption := connectPackage.Ident("HandlerOption")
	g.P("func ", names.ServerConstructor, "(svc ", names.Server, ", opts ...", handlerOption,
		") (string, ", httpPackage.Ident("Handler"), ") {")
	g.P("mux := ", httpPackage.Ident("NewServeMux"), "()")
	for _, method := range service.Methods {
		isStreamingServer := method.Desc.IsStreamingServer()
		isStreamingClient := method.Desc.IsStreamingClient()
		if isStreamingClient && !isStreamingServer {
			g.P(`mux.Handle("`, procedureName(method), `", `, connectPackage.Ident("NewClientStreamHandler"), "(")
		} else if !isStreamingClient && isStreamingServer {
			g.P(`mux.Handle("`, procedureName(method), `", `, connectPackage.Ident("NewServerStreamHandler"), "(")
		} else if isStreamingClient && isStreamingServer {
			g.P(`mux.Handle("`, procedureName(method), `", `, connectPackage.Ident("NewBidiStreamHandler"), "(")
		} else {
			g.P(`mux.Handle("`, procedureName(method), `", `, connectPackage.Ident("NewUnaryHandler"), "(")
		}
		g.P(`"`, procedureName(method), `",`)
		g.P("svc.", method.GoName, ",")
		g.P("opts...,")
		g.P("))")
	}
	g.P(`return "/`, reflectionName(service), `/", mux`)
	g.P("}")
	g.P()
}

func unimplementedServerImplementation(g *protogen.GeneratedFile, service *protogen.Service, names names) {
	wrapComments(g, names.UnimplementedServer, " returns CodeUnimplemented from all methods.")
	g.P("type ", names.UnimplementedServer, " struct {}")
	g.P()
	for _, method := range service.Methods {
		g.P("func (", names.UnimplementedServer, ") ", serverSignature(g, method), "{")
		if method.Desc.IsStreamingServer() || method.Desc.IsStreamingClient() {
			g.P("return ", connectPackage.Ident("NewError"), "(",
				connectPackage.Ident("CodeUnimplemented"), ", ", errorsPackage.Ident("New"),
				`("`, method.Desc.FullName(), ` isn't implemented"))`)
		} else {
			g.P("return nil, ", connectPackage.Ident("NewError"), "(",
				connectPackage.Ident("CodeUnimplemented"), ", ", errorsPackage.Ident("New"),
				`("`, method.Desc.FullName(), ` isn't implemented"))`)
		}
		g.P("}")
		g.P()
	}
	g.P()
}

func unexport(s string) string { return strings.ToLower(s[:1]) + s[1:] }

func deprecated(g *protogen.GeneratedFile) {
	g.P("// Deprecated: do not use.")
}

func leadingComments(g *protogen.GeneratedFile, comments protogen.Comments, isDeprecated bool) {
	if comments.String() != "" {
		g.P(strings.TrimSpace(comments.String()))
	}
	if isDeprecated {
		if comments.String() != "" {
			g.P("//")
		}
		deprecated(g)
	}
}

// Raggedy comments in the generated code are driving me insane. This
// word-wrapping function is ruinously inefficient, but it gets the job done.
func wrapComments(g *protogen.GeneratedFile, elems ...any) {
	text := &bytes.Buffer{}
	for _, el := range elems {
		switch el := el.(type) {
		case protogen.GoIdent:
			fmt.Fprint(text, g.QualifiedGoIdent(el))
		default:
			fmt.Fprint(text, el)
		}
	}
	words := strings.Fields(text.String())
	text.Reset()
	var pos int
	for _, word := range words {
		n := utf8.RuneCountInString(word)
		if pos > 0 && pos+n+1 > commentWidth {
			g.P("// ", text.String())
			text.Reset()
			pos = 0
		}
		if pos > 0 {
			text.WriteRune(' ')
			pos += 1
		}
		text.WriteString(word)
		pos += n
	}
	if text.Len() > 0 {
		g.P("// ", text.String())
	}
}
