package database

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/autom8ter/graphik/gen/go/api"
	"github.com/autom8ter/graphik/helpers"
	"github.com/autom8ter/graphik/logger"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_auth "github.com/grpc-ecosystem/go-grpc-middleware/auth"
	"github.com/lestrrat-go/jwx/jwa"
	"github.com/lestrrat-go/jwx/jws"
	"github.com/pkg/errors"
	"go.etcd.io/bbolt"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"io/ioutil"
	"net/http"
	"time"
)

const (
	authCtxKey   = "x-graphik-auth-ctx"
	identityType = "identity"
	emailClaim   = "email"
	methodCtxKey = "x-grpc-full-method"
)

func (g *Graph) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		bearer, err := grpc_auth.AuthFromMD(ctx, "Bearer")
		if err != nil {
			return nil, err
		}
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Errorf(codes.Unauthenticated, "empty X-GRAPHIK-ID")
		}
		values := md.Get("X-GRAPHIK-ID")
		if len(values) == 0 {
			return nil, status.Errorf(codes.Unauthenticated, "empty X-GRAPHIK-ID")
		}
		idToken := values[0]
		idTokenHash := helpers.Hash([]byte(idToken))
		if val, ok := g.cache.Get(idTokenHash); ok {
			payload := val.(map[string]interface{})
			ctx, err := g.check(ctx, info.FullMethod, req, payload)
			if err != nil {
				return nil, err
			}
			return handler(ctx, req)
		}
		payload, err := g.verifyJWT(idToken)
		if err != nil {
			return nil, status.Errorf(codes.Unauthenticated, err.Error())
		}
		var exp int64
		if val, ok := payload["exp"].(int64); ok {
			if val < time.Now().Unix() {
				return nil, status.Errorf(codes.Unauthenticated, "token expired")
			}
			exp = val
		}
		if val, ok := payload["exp"].(int); ok {
			if int64(val) < time.Now().Unix() {
				return nil, status.Errorf(codes.Unauthenticated, "token expired")
			}
			exp = int64(val)
		}
		ctx = g.methodToContext(ctx, info.FullMethod)
		if g.openID != nil && g.openID.UserinfoEndpoint != "" {
			req, err := http.NewRequest(http.MethodGet, g.openID.UserinfoEndpoint, nil)
			if err != nil {
				return nil, status.Errorf(codes.Internal, err.Error())
			}
			req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", bearer))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return nil, status.Errorf(codes.Internal, err.Error())
			}
			defer resp.Body.Close()
			bits, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				return nil, status.Errorf(codes.Internal, err.Error())
			}
			data := map[string]interface{}{}
			if err := json.Unmarshal(bits, &data); err != nil {
				return nil, status.Errorf(codes.Internal, err.Error())
			}
			payload = data
		}
		g.cache.Set(idTokenHash, payload, time.Unix(exp, 0).Sub(time.Now()))
		ctx, err = g.check(ctx, info.FullMethod, req, payload)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

func (g *Graph) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		bearer, err := grpc_auth.AuthFromMD(ss.Context(), "Bearer")
		if err != nil {
			return err
		}
		md, ok := metadata.FromIncomingContext(ss.Context())
		if !ok {
			return status.Errorf(codes.Unauthenticated, "empty X-GRAPHIK-ID")
		}
		values := md.Get("X-GRAPHIK-ID")
		if len(values) == 0 {
			return status.Errorf(codes.Unauthenticated, "empty X-GRAPHIK-ID")
		}
		idToken := values[0]
		idTokenHash := helpers.Hash([]byte(idToken))
		if val, ok := g.cache.Get(idTokenHash); ok {
			payload := val.(map[string]interface{})
			ctx, err := g.check(ss.Context(), info.FullMethod, srv, payload)
			if err != nil {
				return err
			}
			wrapped := grpc_middleware.WrapServerStream(ss)
			wrapped.WrappedContext = ctx
			return handler(srv, wrapped)
		}
		payload, err := g.verifyJWT(idToken)
		if err != nil {
			return status.Errorf(codes.Unauthenticated, err.Error())
		}
		var exp int64
		if val, ok := payload["exp"].(int64); ok {
			if val < time.Now().Unix() {
				return status.Errorf(codes.Unauthenticated, "token expired")
			}
			exp = val
		}
		if val, ok := payload["exp"].(int); ok {
			if int64(val) < time.Now().Unix() {
				return status.Errorf(codes.Unauthenticated, "token expired")
			}
			exp = int64(val)
		}
		if g.openID != nil && g.openID.UserinfoEndpoint != "" {
			req, err := http.NewRequest(http.MethodGet, g.openID.UserinfoEndpoint, nil)
			if err != nil {
				return status.Errorf(codes.Internal, err.Error())
			}
			req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", bearer))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return status.Errorf(codes.Internal, err.Error())
			}
			defer resp.Body.Close()
			bits, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				return status.Errorf(codes.Internal, err.Error())
			}
			data := map[string]interface{}{}
			if err := json.Unmarshal(bits, &data); err != nil {
				return status.Errorf(codes.Internal, err.Error())
			}
			payload = data
		}
		g.cache.Set(idTokenHash, payload, time.Unix(exp, 0).Sub(time.Now()))
		ctx, err := g.check(ss.Context(), info.FullMethod, srv, payload)
		if err != nil {
			return err
		}
		wrapped := grpc_middleware.WrapServerStream(ss)
		wrapped.WrappedContext = ctx
		return handler(srv, wrapped)
	}
}

func (a *Graph) identityToContext(ctx context.Context, payload map[string]interface{}) (context.Context, *apipb.Doc, error) {
	path := &apipb.Path{
		Gtype: identityType,
		Gid:   payload[emailClaim].(string),
	}
	var (
		doc apipb.Doc
		err error
	)
	if err = a.db.View(func(tx *bbolt.Tx) error {
		getDoc, err := a.getDoc(ctx, tx, path)
		if err != nil {
			return err
		}
		doc = *getDoc
		return err
	}); err != nil && err != ErrNotFound {
		return ctx, nil, err
	}
	if doc.GetPath() == nil {
		logger.Info("creating identity",
			zap.String("gtype", path.GetGtype()),
			zap.String("gid", path.GetGid()),
		)
		strct, err := structpb.NewStruct(payload)
		if err != nil {
			return nil, nil, err
		}
		docP, err := a.createIdentity(ctx, &apipb.DocConstructor{
			Path:       path,
			Attributes: strct,
		})
		if err != nil {
			return nil, nil, err
		}
		doc = *docP
	}
	if doc.GetPath() == nil {
		return nil, nil, errors.New("empty doc")
	}
	return context.WithValue(ctx, authCtxKey, &doc), &doc, nil
}

func (s *Graph) getIdentity(ctx context.Context) *apipb.Doc {
	val, ok := ctx.Value(authCtxKey).(*apipb.Doc)
	if ok {
		return val
	}
	val2, ok := ctx.Value(authCtxKey).(apipb.Doc)
	if ok {
		return &val2
	}
	return nil
}

func (r *Graph) getMethod(ctx context.Context) string {
	val, ok := ctx.Value(methodCtxKey).(string)
	if ok {
		return val
	}
	return ""
}

func (r *Graph) methodToContext(ctx context.Context, path string) context.Context {
	return context.WithValue(ctx, methodCtxKey, path)
}

func (g *Graph) verifyJWT(token string) (map[string]interface{}, error) {
	message, err := jws.ParseString(token)
	if err != nil {
		return nil, err
	}
	g.jwksMu.RLock()
	defer g.jwksMu.RUnlock()
	if g.jwksSet == nil {
		data := map[string]interface{}{}
		if err := json.Unmarshal(message.Payload(), &data); err != nil {
			return nil, err
		}
		return data, nil
	}
	if len(message.Signatures()) == 0 {
		return nil, fmt.Errorf("zero jws signatures")
	}
	kid, ok := message.Signatures()[0].ProtectedHeaders().Get("kid")
	if !ok {
		return nil, fmt.Errorf("jws kid not found")
	}
	algI, ok := message.Signatures()[0].ProtectedHeaders().Get("alg")
	if !ok {
		return nil, fmt.Errorf("jw alg not found")
	}
	alg, ok := algI.(jwa.SignatureAlgorithm)
	if !ok {
		return nil, fmt.Errorf("alg type cast error")
	}
	keys := g.jwksSet.LookupKeyID(kid.(string))
	if len(keys) == 0 {
		return nil, errors.Errorf("failed to lookup kid: %s - zero keys", kid.(string))
	}
	var key interface{}
	if err := keys[0].Raw(&key); err != nil {
		return nil, err
	}
	payload, err := jws.Verify([]byte(token), alg, key)
	if err != nil {
		return nil, err
	}
	data := map[string]interface{}{}
	if err := json.Unmarshal(payload, &data); err != nil {
		return nil, err
	}
	return data, nil
}

func (g *Graph) check(ctx context.Context, method string, req interface{}, payload map[string]interface{}) (context.Context, error) {
	ctx = g.methodToContext(ctx, method)
	ctx, identity, err := g.identityToContext(ctx, payload)
	if err != nil {
		return nil, status.Errorf(codes.Internal, err.Error())
	}
	if len(g.authorizers) > 0 {
		now := time.Now()
		request := &apipb.Request{
			Method:    method,
			Identity:  identity,
			Timestamp: timestamppb.New(now),
		}
		if val, ok := req.(proto.Message); ok {
			bits, _ := helpers.MarshalJSON(val)
			reqMap := map[string]interface{}{}
			if err := json.Unmarshal(bits, &reqMap); err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			request.Request = apipb.NewStruct(reqMap)
		}
		result, err := g.vm.Auth().Eval(request, g.authorizers...)
		if err != nil {
			return nil, err
		}
		if !result {
			return nil, status.Error(codes.PermissionDenied, "request authorization = denied")
		}
	}
	if g.getIdentity(ctx) == nil {
		return nil, status.Error(codes.Internal, "empty identity")
	}
	return ctx, nil
}